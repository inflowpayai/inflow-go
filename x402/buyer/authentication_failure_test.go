package buyer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
	foundationhttp "github.com/x402-foundation/x402/go/v2/http"
)

type authExtension struct {
	hook foundationhttp.PaymentRequiredHook
}

func (e authExtension) Key() string                                             { return "test-auth" }
func (e authExtension) PaymentRequiredHook() foundationhttp.PaymentRequiredHook { return e.hook }
func (e authExtension) EnrichPaymentPayload(_ context.Context, payload foundation.PaymentPayload, _ foundation.PaymentRequired) (foundation.PaymentPayload, error) {
	return payload, nil
}

type payloadOnlyExtension struct{ foundation.ClientExtension }

func (e payloadOnlyExtension) Key() string { return "payload-only" }

type authTransport func(*http.Request) (*http.Response, error)

func (f authTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthenticationRejectionFallsBackToOnePayment(t *testing.T) {
	var requests, creates, hooks int
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transactions/x402-supported":
			io.WriteString(w, `{"kinds":[{"x402Version":2,"scheme":"balance","network":"inflow:1"}]}`)
		case "/v1/transactions/x402":
			creates++
			var input struct{ Accept x402.PaymentRequirements }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Accept.Amount != "2" {
				t.Error("paid stale offer", input.Accept.Amount)
			}
			io.WriteString(w, `{"transactionId":"txn","approvalId":"approval","approvalStatus":"APPROVED"}`)
		case "/v1/transactions/txn/x402":
			io.WriteString(w, `{"status":"PENDING","encodedPayload":"payment-proof","paymentPayload":{"x402Version":2,"accepted":{"scheme":"balance","network":"inflow:1","amount":"2"},"payload":{}}}`)
		default:
			t.Error("unexpected platform call", r.URL.Path)
		}
	}))
	defer platform.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer application" {
			t.Error("application authentication changed")
		}
		if requests == 3 {
			if r.Header.Get("Payment-Signature") != "payment-proof" || r.Header.Get("Test-Auth") != "" {
				t.Error("incorrect paid replay headers")
			}
			w.Header().Set("Payment-Response", "receipt")
			w.WriteHeader(500)
			return
		}
		if requests == 2 && r.Header.Get("Test-Auth") != "proof" {
			t.Error("missing authentication proof")
		}
		amount := "1"
		if requests == 2 {
			amount = "2"
		}
		required := x402.PaymentRequired{X402Version: 2, Resource: &x402.ResourceInfo{URL: "https://seller.example"}, Accepts: []x402.PaymentRequirements{{Scheme: "balance", Network: "inflow:1", Amount: amount}}, Extensions: map[string]any{"test-auth": map[string]any{}}}
		raw, err := json.Marshal(required)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Payment-Required", base64.StdEncoding.EncodeToString(raw))
		w.WriteHeader(402)
	}))
	defer resource.Close()
	external := foundation.Newx402Client()
	external.RegisterExtension(authExtension{func(context.Context, foundation.PaymentRequired, string) (*foundationhttp.PaymentRequiredHookResult, error) {
		hooks++
		return &foundationhttp.PaymentRequiredHookResult{Headers: map[string]string{"Test-Auth": "proof"}}, nil
	}})
	client, err := New(Options{Options: inflow.Options{BaseURL: platform.URL}, External: external})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("GET", resource.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer application")
	response, err := client.Do(request, SignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if requests != 3 || creates != 1 || hooks != 1 || response.StatusCode != 500 || response.Header.Get("Payment-Response") != "receipt" {
		t.Fatalf("unexpected retry lifecycle: requests=%d payments=%d hooks=%d status=%d", requests, creates, hooks, response.StatusCode)
	}
}

func TestAuthenticationHooksAreOptionalAndIsolated(t *testing.T) {
	sentinel := errors.New("hook failed")
	for _, scenario := range []string{"undeclared", "payload-only", "nil-hook", "nil-result", "empty-result", "error", "invalid-copy", "isolated"} {
		t.Run(scenario, func(t *testing.T) {
			external := foundation.Newx402Client()
			required := x402.PaymentRequired{X402Version: 2, Extensions: map[string]any{"test-auth": map[string]any{"value": "original"}, "payload-only": true}}
			calls := 0
			hook := func(_ context.Context, value foundation.PaymentRequired, _ string) (*foundationhttp.PaymentRequiredHookResult, error) {
				calls++
				value.Extensions["test-auth"] = "mutated"
				if scenario == "error" {
					return nil, sentinel
				}
				if scenario == "nil-result" {
					return nil, nil
				}
				return &foundationhttp.PaymentRequiredHookResult{}, nil
			}
			if scenario == "undeclared" {
				required.Extensions = nil
			}
			if scenario == "invalid-copy" {
				required.Extensions["invalid"] = make(chan int)
			}
			if scenario == "nil-hook" {
				hook = nil
			}
			if scenario == "payload-only" {
				external.RegisterExtension(payloadOnlyExtension{})
			} else {
				external.RegisterExtension(authExtension{hook})
			}
			client, _ := New(Options{External: external})
			request, _ := http.NewRequest("GET", "https://seller.example", nil)
			_, err := client.authenticationHeaders(request, required)
			if scenario == "error" {
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			} else if scenario == "invalid-copy" {
				if err == nil || calls != 0 {
					t.Fatal("invalid input reached hook")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if scenario == "isolated" && required.Extensions["test-auth"].(map[string]any)["value"] != "original" {
				t.Fatal("hook mutated payment terms")
			}
			if (scenario == "undeclared" || scenario == "payload-only" || scenario == "nil-hook") && calls != 0 {
				t.Fatal("unexpected hook call")
			}
		})
	}
}

func TestAuthenticationRetryFailuresAndFreshPaymentTerms(t *testing.T) {
	for _, scenario := range []string{"hook-error", "cancel", "body-error", "transport-error", "invalid-challenge", "fresh-terms"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("stopped")
			calls, hookCalls, paymentCalls := 0, 0, 0
			external := foundation.Newx402Client()
			external.RegisterExtension(authExtension{func(ctx context.Context, _ foundation.PaymentRequired, _ string) (*foundationhttp.PaymentRequiredHookResult, error) {
				hookCalls++
				if scenario == "hook-error" {
					return nil, sentinel
				}
				if scenario == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				return &foundationhttp.PaymentRequiredHookResult{Headers: map[string]string{"Test-Auth": "proof"}}, nil
			}})
			transport := authTransport(func(r *http.Request) (*http.Response, error) {
				if r.Body != nil {
					defer r.Body.Close()
				}
				if r.URL.Host == "platform.example" {
					if r.URL.Path != "/v1/transactions/x402-supported" {
						t.Fatal("unexpected platform request")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"kinds":[{"x402Version":2,"scheme":"balance","network":"inflow:1"}]}`))}, nil
				}
				calls++
				if calls == 2 && scenario == "transport-error" {
					return nil, sentinel
				}
				raw := []byte(`{"x402Version":2,"accepts":[{"scheme":"balance","network":"inflow:1","amount":"1"}],"extensions":{"test-auth":{}}}`)
				if calls == 2 {
					if r.Header.Get("Test-Auth") != "proof" {
						t.Error("missing auth proof")
					}
					if scenario == "invalid-challenge" {
						raw = []byte("invalid")
					} else {
						raw = []byte(strings.Replace(string(raw), `"amount":"1"`, `"amount":"2"`, 1))
					}
				}
				header := make(http.Header)
				header.Set("Payment-Required", base64.StdEncoding.EncodeToString(raw))
				return &http.Response{StatusCode: 402, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			client, _ := New(Options{Options: inflow.Options{BaseURL: "https://platform.example", Transport: transport}, External: external, Policies: []Policy{func(_ context.Context, offers []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
				paymentCalls++
				if offers[0].Amount != "2" {
					t.Error("used stale offer")
				}
				return nil, sentinel
			}}})
			request, _ := http.NewRequestWithContext(ctx, "POST", "https://seller.example", strings.NewReader("body"))
			if scenario == "body-error" {
				request.GetBody = func() (io.ReadCloser, error) { return nil, sentinel }
			}
			_, err := client.Do(request, SignOptions{})
			if err == nil {
				t.Fatal("failure ignored")
			}
			if scenario == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if scenario != "cancel" && scenario != "invalid-challenge" && !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if hookCalls != 1 || calls > 2 {
				t.Fatal("repeated authentication")
			}
			want := 0
			if scenario == "fresh-terms" {
				want = 1
			}
			if paymentCalls != want {
				t.Fatal("unexpected payment")
			}
		})
	}
}

func TestAuthenticationUsesDecodedNumbers(t *testing.T) {
	// An extension receives the same exact numbers retained by the payment decoder.
	required, err := decode[x402.PaymentRequired]([]byte(`{"x402Version":2,"extensions":{"test-auth":{"number":9007199254740993}}}`))
	if err != nil {
		t.Fatal(err)
	}
	external := foundation.Newx402Client()
	external.RegisterExtension(authExtension{func(_ context.Context, value foundation.PaymentRequired, _ string) (*foundationhttp.PaymentRequiredHookResult, error) {
		if value.Extensions["test-auth"].(map[string]any)["number"] != json.Number("9007199254740993") {
			t.Fatal("rounded extension metadata")
		}
		return nil, nil
	}})
	client, _ := New(Options{External: external})
	request, _ := http.NewRequest("GET", "https://seller.example", nil)
	if _, err := client.authenticationHeaders(request, required); err != nil {
		t.Fatal(err)
	}
}
