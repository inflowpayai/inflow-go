package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
)

const transactionID = "22222222-2222-4222-8222-222222222222"
const approvalID = "33333333-3333-4333-8333-333333333333"
const subscriptionID = "44444444-4444-4444-8444-444444444444"

func challenge() mpp.Challenge {
	return mpp.Challenge{ID: "test", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "e30"}
}

func credential(t *testing.T) string {
	t.Helper()
	source := "did:inflow:test"
	value, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenge(), Payload: map[string]any{"transactionId": transactionID, "amount": json.Number("9007199254740993")}, Source: &source})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func newClient(t *testing.T, server *httptest.Server, modify func(*buyer.Options)) *buyer.Client {
	t.Helper()
	options := buyer.Options{Options: inflow.Options{BaseURL: server.URL, APIKey: "test-only-buyer-key"}, PollInterval: time.Millisecond, WaitTimeout: time.Second}
	if modify != nil {
		modify(&options)
	}
	client, err := buyer.New(options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func requireCode(t *testing.T, err error, code buyer.ErrorCode) *buyer.Error {
	t.Helper()
	var payment *buyer.Error
	if !errors.As(err, &payment) || payment.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
	if !strings.Contains(payment.Error(), string(code)) {
		t.Fatal(payment)
	}
	return payment
}

type exchange struct {
	Request struct {
		Method, Path string
		Headers      map[string]string
		JSON         json.RawMessage
	}
	Response struct {
		Status int
		JSON   json.RawMessage
		Delay  int `json:"delay_ms"`
	}
}

func equalJSON(t *testing.T, a, b []byte) {
	t.Helper()
	var first, second any
	if err := json.Unmarshal(a, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("got %s; want %s", a, b)
	}
}

func TestSharedBuyerCases(t *testing.T) {
	data, err := os.ReadFile("testdata/buyer.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Operation string
		Input         struct {
			Challenge mpp.Challenge
			Context   struct{ InstrumentID, SubscriptionID string }
			Timeout   int `json:"timeout_ms"`
		}
		Expect struct {
			Result json.RawMessage
			Error  *struct {
				Code    string
				Details struct {
					Problem       json.RawMessage
					TransactionID string `json:"transaction_id"`
				}
			}
		}
		Platform struct{ Exchanges []exchange }
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 25 {
		t.Fatalf("unexpected fixture count %d", len(cases))
	}
	for _, test := range cases {
		t.Run(test.ID, func(t *testing.T) {
			var index atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(index.Add(1)) - 1
				if i >= len(test.Platform.Exchanges) {
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					w.WriteHeader(500)
					return
				}
				e := test.Platform.Exchanges[i]
				if r.Method != e.Request.Method || r.URL.Path != e.Request.Path {
					t.Errorf("got %s %s; want %s %s", r.Method, r.URL.Path, e.Request.Method, e.Request.Path)
				}
				for key, value := range e.Request.Headers {
					if r.Header.Get(key) != value {
						t.Errorf("missing header %s", key)
					}
				}
				body, _ := io.ReadAll(r.Body)
				if len(e.Request.JSON) > 0 {
					equalJSON(t, body, e.Request.JSON)
				} else if len(body) > 0 {
					t.Error("unexpected request body")
				}
				if e.Response.Delay > 0 {
					select {
					case <-time.After(time.Duration(e.Response.Delay) * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				w.WriteHeader(e.Response.Status)
				_, _ = w.Write(e.Response.JSON)
			}))
			defer server.Close()
			client := newClient(t, server, func(options *buyer.Options) {
				if test.Input.Timeout > 0 {
					options.WaitTimeout = time.Duration(test.Input.Timeout) * time.Millisecond
				}
			})
			options := buyer.PaymentOptions{InstrumentID: test.Input.Context.InstrumentID, SubscriptionID: test.Input.Context.SubscriptionID}
			var result mpp.Credential
			var err error
			if test.Operation == "mpp.buyer.cancel" {
				payment, prepareErr := client.Prepare(context.Background(), test.Input.Challenge, options)
				if prepareErr != nil {
					t.Fatal(prepareErr)
				}
				if err = payment.Cancel(context.Background()); err != nil {
					t.Fatal(err)
				}
				result, err = payment.Wait(context.Background())
			} else {
				result, err = client.Fulfil(context.Background(), test.Input.Challenge, options)
			}
			if test.Expect.Error != nil {
				payment := requireCode(t, err, buyer.ErrorCode(test.Expect.Error.Code))
				if test.Expect.Error.Details.TransactionID != "" && payment.TransactionID != test.Expect.Error.Details.TransactionID {
					t.Fatal(payment)
				}
				if len(test.Expect.Error.Details.Problem) > 0 {
					equalJSON(t, payment.Problem, test.Expect.Error.Details.Problem)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(result)
				equalJSON(t, encoded, test.Expect.Result)
			}
			if int(index.Load()) != len(test.Platform.Exchanges) {
				t.Fatalf("executed %d of %d exchanges", index.Load(), len(test.Platform.Exchanges))
			}
		})
	}
}

func TestPollBeyondBudgetWaitsForTimeoutOrCancellation(t *testing.T) {
	for _, test := range []struct {
		name     string
		advice   string
		poll     time.Duration
		cancel   bool
		wantPoll bool
	}{
		{name: "server delay", advice: `,"retryAfterSeconds":60`, poll: time.Millisecond},
		{name: "default delay", poll: time.Second},
		{name: "equal delay", poll: 50 * time.Millisecond},
		{name: "cancel during delay", advice: `,"retryAfterSeconds":60`, poll: time.Millisecond, cancel: true},
		{name: "cancel before scheduled poll", poll: 500 * time.Millisecond, cancel: true},
		{name: "poll before deadline", advice: `,"retryAfterSeconds":0`, poll: time.Millisecond, wantPoll: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := credential(t)
			var polls, cancellations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/transactions/mpp":
					fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q%s}`, transactionID, approvalID, test.advice)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/approvals/"+approvalID+"/cancel":
					if r.Context().Err() != nil {
						t.Error("cleanup used a cancelled request context")
					}
					cancellations.Add(1)
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/transactions/"+transactionID+"/mpp":
					polls.Add(1)
					fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			client := newClient(t, server, func(options *buyer.Options) {
				options.PollInterval = test.poll
				options.WaitTimeout = 50 * time.Millisecond
				if test.cancel || test.wantPoll {
					options.WaitTimeout = time.Second
				}
			})
			payment, err := client.Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				timer := time.AfterFunc(10*time.Millisecond, cancel)
				defer timer.Stop()
			}
			result, err := payment.Wait(ctx)
			if test.wantPoll {
				if err != nil || result.Payload["transactionId"] != transactionID || polls.Load() != 1 || cancellations.Load() != 0 {
					t.Fatalf("successful poll: result=%v error=%v polls=%d cancellations=%d", result, err, polls.Load(), cancellations.Load())
				}
				return
			}
			code, cause := buyer.Timeout, context.DeadlineExceeded
			if test.cancel {
				code, cause = buyer.Cancelled, context.Canceled
			}
			failure := requireCode(t, err, code)
			if !errors.Is(err, cause) || failure.TransactionID != transactionID || failure.ApprovalID != approvalID {
				t.Fatal(failure)
			}
			if polls.Load() != 0 || cancellations.Load() != 1 {
				t.Fatalf("polls=%d cancellations=%d", polls.Load(), cancellations.Load())
			}
		})
	}
}

func TestValidationBeforeNetwork(t *testing.T) {
	for _, options := range []buyer.Options{{PollInterval: -1}, {WaitTimeout: -1}, {Options: inflow.Options{Environment: "bad"}}} {
		if _, err := buyer.New(options); err == nil {
			t.Fatal("accepted invalid options")
		}
	}
	client, err := buyer.New(buyer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ method, intent, id string }{
		{"other", "charge", ""}, {"inflow", "other", ""}, {"tempo", "subscription", ""},
		{"inflow", "charge", "invalid"}, {"inflow", "charge", "22222222x2222-4222-8222-222222222222"}, {"inflow", "charge", "g2222222-2222-4222-8222-222222222222"},
	} {
		c := challenge()
		c.Method = test.method
		c.Intent = test.intent
		if _, err := client.Fulfil(context.Background(), c, buyer.PaymentOptions{InstrumentID: test.id}); err == nil {
			t.Fatal(test)
		}
	}
	c := challenge()
	c.ID = ""
	if _, err := client.Prepare(context.Background(), c, buyer.PaymentOptions{}); err == nil {
		t.Fatal("missing ID")
	}
	c = challenge()
	if _, err := client.Prepare(context.Background(), c, buyer.PaymentOptions{SubscriptionID: subscriptionID}); err == nil {
		t.Fatal("wrong option")
	}
	c.Method = "tempo"
	if _, err := client.Prepare(context.Background(), c, buyer.PaymentOptions{InstrumentID: subscriptionID}); err == nil {
		t.Fatal("wrong method option")
	}
}

func TestConcurrentWaitsAndIndependentResults(t *testing.T) {
	encoded := credential(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == "POST" {
			fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q}`, transactionID, approvalID)
		} else {
			fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
		}
	}))
	defer server.Close()
	client := newClient(t, server, nil)
	payment, err := client.Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if payment.TransactionID() != transactionID || payment.ApprovalID() != approvalID {
		t.Fatal("missing progress IDs")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			result, err := payment.Wait(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			result.Payload["caller-owned"] = "changed"
		})
	}
	wg.Wait()
	result, err := payment.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Payload["caller-owned"] != nil || result.Payload["amount"] != json.Number("9007199254740993") {
		t.Fatal(result)
	}
	if err := payment.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicated workflow: %d", calls.Load())
	}
}

func TestInFlightCancellation(t *testing.T) {
	for _, phase := range []string{"create", "poll", "authorize"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			disconnected := make(chan struct{})
			var cancellations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					cancellations.Add(1)
					w.WriteHeader(204)
					return
				}
				if phase == "poll" && r.Method == "POST" {
					fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":0}`, transactionID, approvalID)
					return
				}
				close(entered)
				<-r.Context().Done()
				close(disconnected)
			}))
			defer server.Close()
			client := newClient(t, server, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := challenge()
			options := buyer.PaymentOptions{}
			if phase == "authorize" {
				c.Intent = "subscription"
				options.SubscriptionID = subscriptionID
			}
			done := make(chan error, 1)
			go func() { _, err := client.Fulfil(ctx, c, options); done <- err }()
			<-entered
			cancel()
			err := <-done
			requireCode(t, err, buyer.Cancelled)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			select {
			case <-disconnected:
			case <-time.After(time.Second):
				t.Fatal("HTTP request not interrupted")
			}
			want := int32(0)
			if phase == "poll" {
				want = 1
			}
			if cancellations.Load() != want {
				t.Fatalf("cancel count %d", cancellations.Load())
			}
		})
	}
}

func TestCreationAndPollingFailures(t *testing.T) {
	for _, phase := range []string{"create", "poll"} {
		for _, response := range []string{"{", `null`, `{"state":"unknown"}`, `{"state":"pending","transactionId":"id","retryAfterSeconds":-1}`, "http-error"} {
			t.Run(phase+response, func(t *testing.T) {
				var creates, cancels atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/cancel") {
						cancels.Add(1)
						w.WriteHeader(503)
						return
					}
					if r.Method == "POST" {
						creates.Add(1)
						if phase == "poll" {
							fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":0}`, transactionID, approvalID)
							return
						}
					}
					if response == "http-error" {
						w.WriteHeader(503)
						return
					}
					fmt.Fprint(w, response)
				}))
				defer server.Close()
				_, err := newClient(t, server, nil).Fulfil(context.Background(), challenge(), buyer.PaymentOptions{})
				if response == "http-error" {
					var api *inflow.APIError
					if !errors.As(err, &api) || api.HTTPStatus != 503 {
						t.Fatal(err)
					}
				} else {
					requireCode(t, err, buyer.InvalidResponse)
				}
				if creates.Load() != 1 {
					t.Fatal("replayed creation")
				}
				if phase == "poll" && cancels.Load() != 1 {
					t.Fatal("missing cleanup")
				}
			})
		}
	}
}

func TestWaitCancellationAndCancelFailure(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			close(entered)
			<-release
			w.WriteHeader(500)
			return
		}
		fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":60}`, transactionID, approvalID)
	}))
	defer server.Close()
	payment, err := newClient(t, server, nil).Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waited := make(chan error, 1)
	go func() { _, err := payment.Wait(ctx); waited <- err }()
	<-entered
	if err := payment.Cancel(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	requireCode(t, <-waited, buyer.Cancelled)
	var api *inflow.APIError
	if err := payment.Cancel(context.Background()); !errors.As(err, &api) || api.HTTPStatus != 500 {
		t.Fatal(err)
	}
}

func TestCleanupBudgetAndAuthenticationContext(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "value")
	var seen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			<-r.Context().Done()
			return
		}
		fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":60}`, transactionID, approvalID)
	}))
	defer server.Close()
	client := newClient(t, server, func(options *buyer.Options) {
		options.APIKey = ""
		options.AccessToken = func(ctx context.Context) (string, error) {
			if ctx.Value(key{}) != "value" {
				t.Error("authentication context lost")
			}
			if seen.Swap(true) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Error("cleanup not bounded")
				}
			}
			return "test-token", nil
		}
	})
	payment, err := client.Prepare(parent, challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = payment.Cancel(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("cleanup took %s", elapsed)
	}
	_, err = payment.Wait(context.Background())
	requireCode(t, err, buyer.Cancelled)
}

func TestPaymentsRemainIndependent(t *testing.T) {
	encoded := credential(t)
	var cancelled atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			cancelled.Add(1)
			w.WriteHeader(204)
			return
		}
		if r.Method == "POST" {
			fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":0}`, transactionID, approvalID)
		} else {
			fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
		}
	}))
	defer server.Close()
	client := newClient(t, server, nil)
	first, err := client.Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := first.Cancel(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Fulfil(context.Background(), challenge(), buyer.PaymentOptions{}); err != nil {
		t.Fatal(err)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("cleanup count %d", cancelled.Load())
	}
}

func TestCallerDeadlineAndDelayedWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			w.WriteHeader(204)
			return
		}
		fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":60}`, transactionID, approvalID)
	}))
	defer server.Close()
	client := newClient(t, server, func(o *buyer.Options) { o.WaitTimeout = 20 * time.Millisecond })
	payment, err := client.Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	_, err = payment.Wait(context.Background())
	requireCode(t, err, buyer.Timeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client = newClient(t, server, nil)
	_, err = client.Fulfil(ctx, challenge(), buyer.PaymentOptions{})
	requireCode(t, err, buyer.Cancelled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestLatestApprovalIsCancelled(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			if polls.Add(1) == 1 {
				fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":0}`, transactionID, approvalID)
			} else {
				fmt.Fprint(w, `{"state":"failed"}`)
			}
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			if r.URL.Path != "/v1/approvals/"+approvalID+"/cancel" {
				t.Error(r.URL.Path)
			}
			w.WriteHeader(204)
		default:
			fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"retryAfterSeconds":0}`, transactionID)
		}
	}))
	defer server.Close()
	_, err := newClient(t, server, nil).Fulfil(context.Background(), challenge(), buyer.PaymentOptions{})
	requireCode(t, err, buyer.Failed)
	if polls.Load() != 2 {
		t.Fatal("did not poll twice")
	}
}

func TestCancellationDuringAdvisedWait(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			w.WriteHeader(204)
			return
		}
		if r.Method == "GET" {
			polls.Add(1)
		}
		fmt.Fprintf(w, `{"state":"pending","transactionId":%q,"approvalId":%q,"retryAfterSeconds":60}`, transactionID, approvalID)
	}))
	defer server.Close()
	client := newClient(t, server, nil)
	payment, err := client.Prepare(context.Background(), challenge(), buyer.PaymentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = payment.Wait(ctx)
	requireCode(t, err, buyer.Cancelled)
	if polls.Load() != 0 {
		t.Fatal("polled before the advised delay")
	}
}
