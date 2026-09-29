package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

func TestSharedBuyerCases(t *testing.T) {
	data, err := os.ReadFile("testdata/buyer.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Operation string
		Input         struct {
			APIKey      string `json:"api_key"`
			Requirement x402.PaymentRequirements
			Context     x402.PaymentRequired
			PaymentID   string `json:"payment_id"`
			Timeout     int    `json:"timeout_ms"`
			Interval    int    `json:"poll_interval_ms"`
		}
		Expect struct {
			Result json.RawMessage
			Error  *struct {
				Code       string
				HTTPStatus int `json:"http_status"`
				Details    struct {
					Status string
					Body   any
				}
			}
		}
		Platform struct {
			Exchanges []struct {
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
		}
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 24 {
		t.Fatalf("unexpected suite length %d", len(cases))
	}
	for _, test := range cases {
		t.Run(test.ID, func(t *testing.T) {
			var mu sync.Mutex
			index := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if index >= len(test.Platform.Exchanges) {
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(500)
					return
				}
				e := test.Platform.Exchanges[index]
				index++
				if r.Method != e.Request.Method || r.URL.Path != e.Request.Path {
					t.Errorf("got %s %s, want %s %s", r.Method, r.URL.Path, e.Request.Method, e.Request.Path)
				}
				for key, value := range e.Request.Headers {
					if r.Header.Get(key) != value {
						t.Errorf("header %s mismatch", key)
					}
				}
				data, _ := io.ReadAll(r.Body)
				if len(e.Request.JSON) > 0 {
					// extra is optional on the wire; compare the upstream typed request semantics.
					var actual, want struct {
						Accept   x402.PaymentRequirements
						Resource x402.ResourceInfo
						Version  int    `json:"x402Version"`
						RemoteID string `json:"remotePaymentId"`
					}
					if err := json.Unmarshal(data, &actual); err != nil {
						t.Error(err)
					}
					if err := json.Unmarshal(e.Request.JSON, &want); err != nil {
						t.Error(err)
					}
					if len(want.Accept.Extra) == 0 {
						want.Accept.Extra = nil
					}
					if len(actual.Accept.Extra) == 0 {
						actual.Accept.Extra = nil
					}
					if !reflect.DeepEqual(actual, want) {
						t.Errorf("request mismatch %s vs %s", data, e.Request.JSON)
					}
				}
				if e.Response.Delay > 0 {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(time.Duration(e.Response.Delay) * time.Millisecond):
					}
				}
				w.WriteHeader(e.Response.Status)
				w.Write(e.Response.JSON)
			}))
			defer s.Close()
			timeout, interval := 2*time.Second, time.Millisecond
			if test.Input.Timeout != 0 {
				timeout = time.Duration(test.Input.Timeout) * time.Millisecond
			}
			if test.Input.Interval != 0 {
				interval = time.Duration(test.Input.Interval) * time.Millisecond
			}
			c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL, APIKey: test.Input.APIKey}, WaitTimeout: timeout, PollInterval: interval})
			if err != nil {
				t.Fatal(err)
			}
			// Node's asynchronous constructor loads capabilities before any operation.
			_, err = c.Supported(context.Background())
			var result buyer.EncodedPayment
			if err == nil {
				required := test.Input.Context
				required.Accepts = []x402.PaymentRequirements{test.Input.Requirement}
				var payment *buyer.Payment
				payment, err = c.Prepare(context.Background(), required, buyer.SignOptions{PaymentID: test.Input.PaymentID})
				if err == nil {
					switch test.Operation {
					case "x402.buyer.cancel":
						_ = payment.Cancel(context.Background())
						result, err = payment.Wait(context.Background())
					case "x402.buyer.concurrent-await":
						type answer struct {
							result buyer.EncodedPayment
							err    error
						}
						ch := make(chan answer, 2)
						for range 2 {
							go func() { v, e := payment.Wait(context.Background()); ch <- answer{v, e} }()
						}
						a, b := <-ch, <-ch
						if !reflect.DeepEqual(a, b) {
							t.Fatal("different concurrent results")
						}
						result, err = a.result, a.err
					case "x402.buyer.sign":
						result, err = payment.Wait(context.Background())
					default:
						t.Fatalf("unknown operation %s", test.Operation)
					}
				}
			}
			if test.Expect.Error == nil {
				if err != nil {
					t.Fatal(err)
				}
				var expected buyer.EncodedPayment
				if e := json.Unmarshal(test.Expect.Result, &expected); e != nil {
					t.Fatal(e)
				}
				actualData, _ := json.Marshal(result)
				expectedData, _ := json.Marshal(expected)
				var actual, want any
				json.Unmarshal(actualData, &actual)
				json.Unmarshal(expectedData, &want)
				if !reflect.DeepEqual(actual, want) {
					t.Errorf("result mismatch %s vs %s", actualData, expectedData)
				}
			} else {
				var paymentErr *buyer.Error
				var apiErr *inflow.APIError
				if errors.As(err, &paymentErr) {
					if paymentErr.Code != test.Expect.Error.Code || paymentErr.Status != test.Expect.Error.Details.Status {
						t.Fatalf("unexpected payment error %+v", paymentErr)
					}
				} else if errors.As(err, &apiErr) {
					if test.Expect.Error.Code != "api-error" || apiErr.HTTPStatus != test.Expect.Error.HTTPStatus || !reflect.DeepEqual(apiErr.Body, test.Expect.Error.Details.Body) {
						t.Fatalf("unexpected API error %+v", apiErr)
					}
				} else {
					t.Fatalf("unexpected error %v", err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if index != len(test.Platform.Exchanges) {
				t.Errorf("used %d/%d exchanges", index, len(test.Platform.Exchanges))
			}
		})
	}
}
