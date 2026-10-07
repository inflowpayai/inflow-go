package buyer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
)

func TestPaymentStatusUsesCallerContext(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.PaymentStatus(ctx, "original", inflow.PaymentStatusOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("status cancellation: %v", err)
	}
}

func TestConfiguration(t *testing.T) {
	for _, options := range []Options{{PollInterval: -1}, {WaitTimeout: -1}, {Options: inflow.Options{BaseURL: "invalid"}}, {Policies: []Policy{nil}}} {
		if _, err := New(options); err == nil {
			t.Fatal("accepted invalid options")
		}
	}
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.options.PollInterval != 5*time.Second || c.options.WaitTimeout != 15*time.Minute || c.options.Timeout != 30*time.Second {
		t.Fatal("wrong defaults")
	}
	sentinel := errors.New("cause")
	e := &Error{Code: "invalid-response", Cause: sentinel}
	if e.Error() != "x402 invalid-response" || !errors.Is(e, sentinel) {
		t.Fatal("error contract")
	}
}

func TestCapabilitySharedLoadAndInvalidation(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var loads atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if loads.Add(1) == 1 {
					close(entered)
					<-release
					if failure {
						fmt.Fprint(w, `{`)
						return
					}
				}
				fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1"}]}`)
			}))
			defer s.Close()
			c, err := New(Options{Options: inflow.Options{BaseURL: s.URL}})
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 2)
			go func() { _, err := c.Supported(context.Background()); result <- err }()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			if _, err := c.Supported(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			cancel()
			// Done is evaluated inside the in-flight select while the HTTP load is blocked.
			joined := make(chan struct{})
			go func() {
				_, err := c.Supported(notifyContext{Context: context.Background(), called: joined})
				result <- err
			}()
			<-joined
			close(release)
			for range 2 {
				err := <-result
				if (failure && err == nil) || (!failure && err != nil) {
					t.Fatal(err)
				}
			}
			value, err := c.Supported(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			value.Kinds[0].Scheme = "mutated"
			again, err := c.Supported(context.Background())
			if err != nil || again.Kinds[0].Scheme != "balance" {
				t.Fatal("shared cache memory")
			}
			c.mu.Lock()
			c.expires = time.Time{}
			c.mu.Unlock()
			if _, err := c.Supported(context.Background()); err != nil {
				t.Fatal(err)
			}
			if loads.Load() < 2 {
				t.Fatal("expired cache not reloaded")
			}
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
			if _, err := c.Supported(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

type notifyContext struct {
	context.Context
	called chan struct{}
}

func (c notifyContext) Done() <-chan struct{} { close(c.called); return nil }

func TestCallbackJSONBoundaries(t *testing.T) {
	bad := x402.PaymentRequired{Extensions: map[string]any{"bad": make(chan int)}}
	c, _ := New(Options{Hooks: []Hooks{{Before: func(context.Context, x402.PaymentRequired) error { return nil }, After: func(context.Context, x402.PaymentRequired, x402.PaymentPayload) error { return nil }, Failure: func(context.Context, x402.PaymentRequired, error) (*x402.PaymentPayload, error) { return nil, nil }}}})
	if err := c.before(context.Background(), bad); err == nil {
		t.Fatal("non-JSON hook input")
	}
	if err := c.after(context.Background(), bad, x402.PaymentPayload{}); err == nil {
		t.Fatal("non-JSON hook input")
	}
	if err := c.after(context.Background(), x402.PaymentRequired{}, x402.PaymentPayload{Payload: map[string]any{"bad": make(chan int)}}); err == nil {
		t.Fatal("non-JSON external payload")
	}
	if _, err := c.failure(context.Background(), bad, errors.New("original")); err == nil || !strings.Contains(err.Error(), "invalid-input") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.after(ctx, x402.PaymentRequired{}, x402.PaymentPayload{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.policies(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	original := errors.New("original")
	if _, err := c.failure(ctx, x402.PaymentRequired{}, original); !errors.Is(err, original) {
		t.Fatal(err)
	}
	if _, err := c.external(context.Background(), bad); err == nil {
		t.Fatal("invalid external input")
	}
}
