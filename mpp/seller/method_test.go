package seller_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	"github.com/inflowpayai/inflow-go/mpp/seller"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	fc "github.com/tempoxyz/mpp-go/pkg/client"
	mc "github.com/tempoxyz/mpp-go/pkg/mcp/client"
	ms "github.com/tempoxyz/mpp-go/pkg/mcp/server"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
	fs "github.com/tempoxyz/mpp-go/pkg/server"
)

// The platform is simulated; challenge issuance, HTTP/MCP transport, lifecycle
// dispatch, and credential parsing use the released upstream implementation.
type methodPlatform struct {
	mu       sync.Mutex
	calls    []string
	failure  string
	issued   *mpp.Credential
	mismatch string
}

func (p *methodPlatform) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *methodPlatform) serve(w http.ResponseWriter, r *http.Request) {
	p.record(r.URL.Path)
	if r.URL.Path == "/v1/mpp/validate" || r.URL.Path == "/v1/mpp/broadcast" {
		p.mu.Lock()
		issued := p.issued
		p.mu.Unlock()
		if issued != nil {
			expected := *issued
			// Upstream ChallengeEcho omits description, as documented in the README.
			// The approval request must still contain it; every other field must survive.
			expected.Challenge.Description = nil
			raw, err := io.ReadAll(r.Body)
			var body struct{ Credential mpp.Credential }
			if err != nil || json.Unmarshal(raw, &body) != nil || !reflect.DeepEqual(body.Credential, expected) {
				p.mu.Lock()
				want, _ := json.Marshal(expected)
				p.mismatch = fmt.Sprintf("got %s; want %s", raw, want)
				p.mu.Unlock()
				http.Error(w, "credential changed in transit", 400)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
	}
	switch r.URL.Path {
	case "/v1/mpp/config":
		fmt.Fprint(w, configJSON)
	case "/v1/transactions/mpp":
		var body struct{ Challenge mpp.Challenge }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if body.Challenge.Description == nil || *body.Challenge.Description != "Paid report" {
			http.Error(w, "missing purchase description", 400)
			return
		}
		source := "did:key:buyer"
		credential := mpp.Credential{Challenge: body.Challenge, Payload: map[string]any{"transactionId": "22222222-2222-4222-8222-222222222222"}, Source: &source}
		p.mu.Lock()
		p.issued = &credential
		p.mu.Unlock()
		encoded, err := mpp.EncodeCredential(credential)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": encoded})
	case "/v1/mpp/validate":
		if p.failure == "validation" {
			fmt.Fprint(w, `{"success":false,"problem":{"type":"about:blank","status":403,"title":"Rejected"}}`)
			return
		}
		writeValidation(w, r)
	case "/v1/mpp/broadcast":
		if p.failure == "pending" {
			fmt.Fprint(w, `{"problem":{"type":"about:blank","status":409,"title":"Pending"}}`)
			return
		}
		if p.failure == "uncertain" {
			http.Error(w, "unavailable", 400)
			return
		}
		fmt.Fprint(w, `{"receipt":{"method":"inflow","reference":"test","status":"success","timestamp":"2026-09-28T00:00:00.123456789Z","challengeId":"test-binding","settlement":{"amount":"1","currency":"USDC"}}}`)
	default:
		http.NotFound(w, r)
	}
}

func methodSetup(t *testing.T, failure string) (*methodPlatform, fs.Method, *fs.Mpp, fc.Method) {
	t.Helper()
	p := &methodPlatform{failure: failure}
	s := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(s.Close)
	sellerClient := clientFor(t, s)
	method, err := sellerClient.Method(context.Background(), chargeOffer())
	if err != nil {
		t.Fatal(err)
	}
	payment, err := fs.New(method, "seller.example", strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	buyerClient, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL, APIKey: "test-only-buyer-key"}})
	if err != nil {
		t.Fatal(err)
	}
	buyerMethod, err := buyerClient.Method("inflow", "charge", buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p, method, payment, buyerMethod
}

func TestUpstreamHTTPMethodLifecycle(t *testing.T) {
	for _, failure := range []string{"", "validation", "pending", "uncertain", "handler"} {
		t.Run(failure, func(t *testing.T) {
			p, _, payment, buyerMethod := methodSetup(t, failure)
			var requests atomic.Int32
			protected := fs.ChargeMiddleware(payment, fs.ChargeParams{Description: "Paid report"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p.mu.Lock()
				settled := reflect.DeepEqual(p.calls, []string{"/v1/mpp/config", "/v1/transactions/mpp", "/v1/mpp/validate", "/v1/mpp/broadcast"})
				p.mu.Unlock()
				if !settled {
					t.Error("handler ran before successful settlement")
				}
				p.record("handler")
				if fs.ReceiptFromContext(r.Context()) == nil {
					t.Error("handler ran without receipt")
				}
				w.Header().Set("X-Application", "retained")
				if failure == "handler" {
					http.Error(w, "failed", 500)
					return
				}
				fmt.Fprint(w, "paid")
			}))
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("X-API-Key") != "" {
					t.Error("platform key reached resource")
				}
				protected.ServeHTTP(w, r)
			}))
			defer resource.Close()
			client, err := fc.NewHTTPClient([]fc.Method{buyerMethod}, fc.WithMaxRetries(1))
			if err != nil {
				t.Fatal(err)
			}
			client.Timeout = 5 * time.Second
			res, err := client.Get(resource.URL + "/report")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			p.mu.Lock()
			mismatch := p.mismatch
			p.mu.Unlock()
			if mismatch != "" {
				t.Fatal(mismatch)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 {
				t.Fatalf("expected one challenge and one paid request, got %d", requests.Load())
			}
			if failure == "" || failure == "handler" {
				wantBody := "paid"
				if failure == "handler" {
					wantBody = "failed\n"
				}
				if string(body) != wantBody || res.Header.Get("X-Application") != "retained" {
					t.Fatalf("application response changed: %q %v", body, res.Header)
				}
			} else if res.Header.Get("Payment-Receipt") != "" {
				t.Fatal("failed payment carried a receipt")
			}
			want := 200
			switch failure {
			case "validation":
				want = 403
			case "pending":
				want = 409
			case "uncertain", "handler":
				want = 500
			}
			if res.StatusCode != want {
				t.Fatalf("status %d, want %d", res.StatusCode, want)
			}
			calls := []string{"/v1/mpp/config", "/v1/transactions/mpp", "/v1/mpp/validate"}
			if failure != "validation" {
				calls = append(calls, "/v1/mpp/broadcast")
			}
			if failure == "" || failure == "handler" {
				calls = append(calls, "handler")
			}
			if failure == "" {
				receipt, err := mpp.DecodeReceipt(res.Header.Get("Payment-Receipt"))
				if err != nil || receipt.ChallengeID == nil || *receipt.ChallengeID != "test-binding" {
					t.Fatalf("receipt lost: %#v %v", receipt, err)
				}
			}
			if failure == "handler" && res.Header.Get("Payment-Receipt") != "" {
				t.Fatal("upstream should omit receipt on handler failure")
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if !reflect.DeepEqual(calls, p.calls) {
				t.Fatalf("calls %v, want %v", p.calls, calls)
			}
		})
	}
}

func TestUpstreamSplitLifecycleAndBindings(t *testing.T) {
	p, method, payment, _ := methodSetup(t, "")
	request, err := method.(fs.ChargeRequestBuilder).BuildChargeRequest(fs.ChargeParams{})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := foundation.NewChallengeWithError(strings.Repeat("s", 32), "seller.example", "inflow", "charge", request, foundation.WithExpires(time.Now().Add(time.Minute).UTC().Format(time.RFC3339)))
	if err != nil {
		t.Fatal(err)
	}
	credential := challenge.NewCredential(map[string]any{"transactionId": "22222222-2222-4222-8222-222222222222"})
	if _, err := payment.ValidateCredential(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 2 || p.calls[1] != "/v1/mpp/validate" {
		t.Fatalf("validation consumed payment: %v", p.calls)
	}
	if _, err := payment.BroadcastCredential(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.calls, []string{"/v1/mpp/config", "/v1/mpp/validate", "/v1/mpp/validate", "/v1/mpp/broadcast"}) {
		t.Fatal(p.calls)
	}
	credential.Challenge.ID = "forged"
	if _, err := payment.BroadcastCredential(context.Background(), credential); err == nil {
		t.Fatal("forged credential accepted")
	}
	if len(p.calls) != 4 {
		t.Fatal("forged credential reached platform")
	}
	intent := method.Intents()["charge"]
	if intent.Name() != "charge" {
		t.Fatal(intent.Name())
	}
	credential = challenge.NewCredential(map[string]any{"transactionId": "22222222-2222-4222-8222-222222222222"})
	if _, err := intent.Verify(context.Background(), credential, request); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamMCPMethods(t *testing.T) {
	for _, scenario := range []string{"success", "declined", "validation", "pending", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			denied := scenario == "declined"
			p, _, payment, buyerMethod := methodSetup(t, scenario)
			server := mcp.NewServer(&mcp.Implementation{Name: "test-seller", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "report", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := ms.Charge(ctx, payment, req, fs.ChargeParams{Description: "Paid report", MppxScope: "tool:report"})
				if err != nil {
					return nil, err
				}
				if result.IsChallenge() {
					return nil, result.Challenge
				}
				p.record("handler")
				return result.WithReceipt(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "paid"}}, Meta: mcp.Meta{"custom": "retained"}}), nil
			})
			client := mc.Wrap(mcp.NewClient(&mcp.Implementation{Name: "test-buyer", Version: "1"}, nil), mc.Config{Methods: []fc.Method{buyerMethod}, OnPaymentRequired: func(context.Context, *foundation.Challenge) (bool, error) { return !denied, nil }})
			ct, st := mcp.NewInMemoryTransports()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := client.Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "report"})
			if denied {
				if !errors.Is(err, fc.ErrPaymentDeclined) {
					t.Fatal(err)
				}
				if len(p.calls) != 1 {
					t.Fatal(p.calls)
				}
				return
			}
			if scenario != "success" {
				if err == nil && (result == nil || !result.IsError) {
					t.Fatal("payment failure released tool result")
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				want := []string{"/v1/mpp/config", "/v1/transactions/mpp", "/v1/mpp/validate"}
				if scenario != "validation" {
					want = append(want, "/v1/mpp/broadcast")
				}
				if !reflect.DeepEqual(p.calls, want) {
					t.Fatal("failure retried payment or ran handler", p.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || result.Meta["custom"] != "retained" || result.Meta[foundation.MCPReceiptMetaKey] == nil {
				t.Fatalf("lost result or receipt: %#v", result)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if !reflect.DeepEqual(p.calls, []string{"/v1/mpp/config", "/v1/transactions/mpp", "/v1/mpp/validate", "/v1/mpp/broadcast", "handler"}) {
				t.Fatal(p.calls)
			}
		})
	}
}

func TestSplitHTTPCompletionChecksCurrentRequest(t *testing.T) {
	for _, scenario := range []string{"matching", "route", "body", "missing-body", "metadata", "missing-metadata"} {
		t.Run(scenario, func(t *testing.T) {
			p, _, payment, _ := methodSetup(t, "")
			ctx := context.Background()
			request, _ := http.NewRequest("POST", "https://seller.example/report", nil)
			params := fs.ChargeParams{MppxScope: fs.ScopeFromHTTPRequest(request, ""), Body: []byte("original"), Meta: map[string]string{"account": "first"}}
			issued, err := payment.Charge(ctx, params)
			if err != nil || issued.Challenge == nil {
				t.Fatalf("challenge: %#v %v", issued, err)
			}
			credential := issued.Challenge.NewCredential(map[string]any{"transactionId": "22222222-2222-4222-8222-222222222222"})
			if _, err := payment.ValidateCredential(ctx, credential); err != nil {
				t.Fatal(err)
			}
			params.Authorization, err = foundation.FormatCredential(credential)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "route":
				params.MppxScope = "POST /other"
			case "body":
				params.Body = []byte("different")
			case "missing-body":
				params.Body = nil
			case "metadata":
				params.Meta = map[string]string{"account": "second"}
			case "missing-metadata":
				params.Meta = nil
			}
			result, err := payment.Charge(ctx, params)
			want := []string{"/v1/mpp/config", "/v1/mpp/validate"}
			if scenario == "matching" {
				if err != nil || result.Receipt == nil {
					t.Fatalf("settlement: %#v %v", result, err)
				}
				want = append(want, "/v1/mpp/validate", "/v1/mpp/broadcast")
			} else if err == nil || result.Receipt != nil {
				t.Fatalf("request mismatch accepted: %#v %v", result, err)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if !reflect.DeepEqual(p.calls, want) {
				t.Fatalf("calls %v, want %v", p.calls, want)
			}
		})
	}
}

func TestUpstreamMethodRejectsOverridesAndMismatches(t *testing.T) {
	p, method, _, _ := methodSetup(t, "")
	builder := method.(fs.ChargeRequestBuilder)
	for _, params := range []fs.ChargeParams{{Amount: "2"}, {Currency: "USD"}, {Recipient: "other"}, {ExternalID: "other"}, {FeePayer: true}, {DisableFeePayer: true}, {FeePayerURL: "https://other.example"}, {ChainID: 1}} {
		if _, err := builder.BuildChargeRequest(params); err == nil {
			t.Fatalf("override accepted: %#v", params)
		}
	}
	request, err := builder.BuildChargeRequest(fs.ChargeParams{})
	if err != nil {
		t.Fatal(err)
	}
	request["amount"] = "modified"
	fresh, err := builder.BuildChargeRequest(fs.ChargeParams{})
	if err != nil || fresh["amount"] != "1" {
		t.Fatal("caller mutated offer", fresh, err)
	}
	challenge, err := foundation.NewChallengeWithError(strings.Repeat("s", 32), "seller.example", "inflow", "charge", fresh)
	if err != nil {
		t.Fatal(err)
	}
	intent := method.Intents()["charge"].(fs.BroadcastingIntent)
	for _, mutate := range []func(*foundation.Credential){
		func(c *foundation.Credential) { c.Challenge.Header = "Payment-Authorization" },
		func(c *foundation.Credential) { c.Challenge.Request = "!" },
		func(c *foundation.Credential) {
			c.Challenge.Request = base64.RawURLEncoding.EncodeToString([]byte(`{"amount":1e9999}`))
		},
		func(c *foundation.Credential) { c.Challenge.Request = "e30" },
		func(c *foundation.Credential) { c.Challenge.Method = "tempo" },
		func(c *foundation.Credential) { c.Challenge.Intent = "subscription" },
	} {
		credential := challenge.NewCredential(map[string]any{"transactionId": "test"})
		mutate(credential)
		if _, err := intent.Broadcast(context.Background(), credential, fresh); err == nil {
			t.Fatal("mismatched credential accepted")
		}
	}
	if _, err := intent.Validate(context.Background(), nil, fresh); err == nil {
		t.Fatal("nil accepted")
	}
	credential := challenge.NewCredential(map[string]any{"transactionId": "test"})
	for _, bad := range []map[string]any{nil, {}, {"bad": make(chan int)}} {
		if _, err := intent.Validate(context.Background(), credential, bad); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if len(p.calls) != 1 {
		t.Fatal("rejected input reached platform", p.calls)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, configJSON) }))
	defer s.Close()
	c := clientFor(t, s)
	if _, err := c.Method(context.Background(), seller.Offer{}); err == nil {
		t.Fatal("empty offer accepted")
	}
	sub, err := c.Method(context.Background(), seller.Offer{Subscription: &mpp.SubscriptionRequest{ChargeRequest: mpp.ChargeRequest{Amount: "1", Currency: "USDC"}, PeriodUnit: "month", PeriodCount: 1, SubscriptionExpires: "2099-01-01T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.(fs.ChargeRequestBuilder).BuildChargeRequest(fs.ChargeParams{}); err == nil {
		t.Fatal("subscription treated as charge")
	}
}

func TestUpstreamValidationDetailsAndFailure(t *testing.T) {
	for _, failure := range []string{"", "invalid-problem", "invalid-status"} {
		t.Run(failure, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/mpp/config" {
					fmt.Fprint(w, configJSON)
					return
				}
				if failure == "invalid-problem" {
					fmt.Fprint(w, `{"success":false,"problem":"bad"}`)
					return
				}
				if failure == "invalid-status" {
					fmt.Fprint(w, `{"success":false,"problem":{"status":200}}`)
					return
				}
				var body struct{ Credential mpp.Credential }
				json.NewDecoder(r.Body).Decode(&body)
				request, _ := mpp.Decode(body.Credential.Challenge.Request)
				json.NewEncoder(w).Encode(map[string]any{"success": true, "credential": body.Credential, "challenge": body.Credential.Challenge, "method": "inflow", "intent": "charge", "request": request, "details": map[string]any{"counter": json.Number("9007199254740993")}})
			}))
			defer s.Close()
			method, err := clientFor(t, s).Method(context.Background(), chargeOffer())
			if err != nil {
				t.Fatal(err)
			}
			request, _ := method.(fs.ChargeRequestBuilder).BuildChargeRequest(fs.ChargeParams{})
			challenge, err := foundation.NewChallengeWithError(strings.Repeat("s", 32), "seller.example", "inflow", "charge", request)
			if err != nil {
				t.Fatal(err)
			}
			value, err := method.Intents()["charge"].(fs.ValidatingIntent).Validate(context.Background(), challenge.NewCredential(map[string]any{"transactionId": "test"}), request)
			if failure != "" {
				if err == nil {
					t.Fatal("rejection accepted")
				}
				return
			}
			if err != nil || value.Details["counter"] != json.Number("9007199254740993") {
				t.Fatalf("details %#v, %v", value, err)
			}
		})
	}
}
