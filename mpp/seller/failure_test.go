package seller_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

func sampleCredential() mpp.Credential {
	return mpp.Credential{Challenge: mpp.Challenge{ID: "test", Method: "inflow", Intent: "charge", Realm: "seller.example", Request: "e30"}, Payload: map[string]any{"transactionId": "test"}}
}

func TestConfigRecoveryAndConcurrentLoad(t *testing.T) {
	for _, first := range []string{"{}", "{", "denied"} {
		t.Run(first, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if first == "denied" {
						w.WriteHeader(403)
					} else {
						fmt.Fprint(w, first)
					}
					return
				}
				fmt.Fprint(w, configJSON)
			}))
			defer server.Close()
			c := clientFor(t, server)
			if c.Load(context.Background()) == nil {
				t.Fatal("accepted failure")
			}
			if err := c.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal(calls.Load())
			}
		})
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		fmt.Fprint(w, configJSON)
	}))
	defer server.Close()
	c := clientFor(t, server)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := c.Load(context.Background()); err != nil {
			t.Error(err)
		}
	})
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Load(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for range 10 {
		wg.Go(func() {
			if err := c.Load(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	if err := c.Load(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestPreparationFailuresAndDefaults(t *testing.T) {
	if _, err := seller.New(inflow.Options{Environment: "invalid"}); err == nil {
		t.Fatal("accepted environment")
	}
	tempo := mpp.TempoRequest{Amount: "1", Currency: "0x1111111111111111111111111111111111111111", Recipient: "0x2222222222222222222222222222222222222222"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, configJSON) }))
	defer server.Close()
	c := clientFor(t, server)
	for _, offer := range []seller.Offer{{}, {Charge: &mpp.ChargeRequest{}, Tempo: &tempo}, {Charge: &mpp.ChargeRequest{}}, {Subscription: &mpp.SubscriptionRequest{}}, {Tempo: &mpp.TempoRequest{Amount: "x"}}, {Tempo: &mpp.TempoRequest{Amount: "1"}}} {
		if _, err := c.Prepare(context.Background(), offer); err == nil {
			t.Fatal("accepted invalid offer")
		}
	}
	p, err := c.Prepare(context.Background(), seller.Offer{Tempo: &tempo})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := mpp.Decode(p.Request)
	details := v.(map[string]any)["methodDetails"].(map[string]any)
	if details["feePayer"] != false || tempo.MethodDetails != nil {
		t.Fatal(v)
	}
	for _, cfg := range []string{
		`{"sellerId":"11111111-1111-4111-8111-111111111111","supportedMethods":[{"id":"other"},{"id":"inflow","methodDetails":{"currencyRails":{"USDC":{"rail":"balance"}}}}]}`,
		`{"sellerId":"bad","supportedMethods":[{"id":"inflow","methodDetails":{"currencyRails":{"USDC":{"rail":"balance"}}}}]}`,
		`{"sellerId":"11111111-1111-4111-8111-111111111111","supportedMethods":[{"id":"inflow"}]}`,
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, cfg) }))
		_, err := clientFor(t, s).Prepare(context.Background(), chargeOffer())
		s.Close()
		if strings.Contains(cfg, `"id":"other"`) {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("accepted bad config")
		}
	}
}

func TestLifecycleFailures(t *testing.T) {
	for _, operation := range []string{"prepare", "tempo", "validate", "broadcast", "verify"} {
		t.Run(operation, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
			defer s.Close()
			c := clientFor(t, s)
			ctx := context.Background()
			var err error
			switch operation {
			case "prepare":
				_, err = c.Prepare(ctx, chargeOffer())
			case "tempo":
				_, err = c.Prepare(ctx, seller.Offer{Tempo: &mpp.TempoRequest{Amount: "1", Currency: "0x1111111111111111111111111111111111111111", Recipient: "0x2222222222222222222222222222222222222222"}})
			case "validate":
				_, err = c.Validate(ctx, sampleCredential())
			case "broadcast":
				_, err = c.Broadcast(ctx, sampleCredential(), "")
			case "verify":
				_, err = c.Verify(ctx, sampleCredential())
			}
			var api *inflow.APIError
			if !errors.As(err, &api) || api.HTTPStatus != 403 {
				t.Fatal(err)
			}
		})
	}
	for _, operation := range []string{"validate", "broadcast"} {
		for _, reply := range []string{"denied", "{", "{} {}"} {
			t.Run(operation+reply, func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/mpp/config" {
						fmt.Fprint(w, configJSON)
						return
					}
					if reply == "denied" {
						w.WriteHeader(403)
					} else {
						fmt.Fprint(w, reply)
					}
				}))
				defer s.Close()
				c := clientFor(t, s)
				var err error
				if operation == "validate" {
					_, err = c.Validate(context.Background(), sampleCredential())
				} else {
					_, err = c.Broadcast(context.Background(), sampleCredential(), "caller-key")
				}
				if err == nil {
					t.Fatal("accepted failure")
				}
			})
		}
	}
	c, _ := seller.New(inflow.Options{})
	for _, value := range []mpp.Credential{{}, func() mpp.Credential { c := sampleCredential(); c.Challenge.Method = "unknown"; return c }(), func() mpp.Credential {
		c := sampleCredential()
		c.Challenge.Method = "tempo"
		c.Payload = map[string]any{"type": 123}
		return c
	}()} {
		for _, broadcast := range []bool{false, true} {
			var err error
			if broadcast {
				_, err = c.Broadcast(context.Background(), value, "")
			} else {
				_, err = c.Validate(context.Background(), value)
			}
			if err == nil {
				t.Fatal("accepted payload")
			}
			if err.Error() == "" {
				t.Fatal("empty error")
			}
		}
	}
}

func TestHTTPPoliciesAndFailures(t *testing.T) {
	for _, mode := range []string{"denied-config", "filter-denied", "filter-error", "bad-header", "rejected-payment", "mutate-offer"} {
		t.Run(mode, func(t *testing.T) {
			var broadcasts atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/mpp/config":
					if mode == "denied-config" {
						w.WriteHeader(403)
					} else {
						fmt.Fprint(w, configJSON)
					}
				case "/v1/mpp/validate":
					fmt.Fprint(w, `{"success":false}`)
				case "/v1/mpp/broadcast":
					broadcasts.Add(1)
				}
			}))
			defer s.Close()
			c := clientFor(t, s)
			route := seller.Route{Realm: "seller.example", SecretKey: "key", Offers: []seller.Offer{chargeOffer()}, CanOffer: func(r *http.Request, o seller.Offer) (bool, error) {
				o.Charge.Amount = "999"
				switch mode {
				case "filter-denied":
					return false, nil
				case "filter-error":
					return false, errors.New("private")
				}
				return true, nil
			}}
			if mode == "bad-header" {
				route.Realm = "bad\nrealm"
			}
			h, err := c.Protect(route, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://seller.example/", nil))
			want := 402
			switch mode {
			case "denied-config", "filter-denied":
				want = 503
			case "filter-error", "bad-header":
				want = 500
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Fatal("error leak")
			}
			if mode == "rejected-payment" || mode == "mutate-offer" {
				challenges, err := mpp.ParseChallenges(w.Header().Values("WWW-Authenticate"))
				if err != nil {
					t.Fatal(err)
				}
				request, _ := mpp.Decode(challenges[0].Request)
				if request.(map[string]any)["amount"] != "1" {
					t.Fatal("offer mutation")
				}
				encoded, _ := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{}})
				r := httptest.NewRequest("GET", "http://seller.example/", nil)
				r.Header.Set("Authorization", "Payment "+encoded)
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 402 || broadcasts.Load() != 0 {
					t.Fatal("invalid accepted")
				}
			}
		})
	}
}

func TestCancelledValidationAndBroadcast(t *testing.T) {
	for _, operation := range []string{"validate", "broadcast"} {
		t.Run(operation, func(t *testing.T) {
			entered, disconnected := make(chan struct{}), make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/mpp/config" {
					fmt.Fprint(w, configJSON)
					return
				}
				close(entered)
				io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
					return
				}
				close(disconnected)
			}))
			defer s.Close()
			c := clientFor(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "validate" {
					_, err = c.Validate(ctx, sampleCredential())
				} else {
					_, err = c.Broadcast(ctx, sampleCredential(), "")
				}
				done <- err
			}()
			<-entered
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			select {
			case <-disconnected:
			case <-time.After(time.Second):
				t.Fatal("request remained active")
			}
		})
	}
}

func TestNoBroadcastRetryWithoutIdempotency(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/mpp/config" {
			fmt.Fprint(w, strings.Replace(configJSON, "true", "false", 1))
			return
		}
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer s.Close()
	_, err := clientFor(t, s).Broadcast(context.Background(), sampleCredential(), "")
	if err == nil || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}
