package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	"github.com/x402-foundation/x402/go/v2/extensions/buildercode"
)

func TestManagedAttribution(t *testing.T) {
	for _, seller := range []bool{false, true} {
		for _, service := range []bool{false, true} {
			for _, prepared := range []bool{false, true} {
				t.Run(fmt.Sprintf("seller=%t/service=%t/prepared=%t", seller, service, prepared), func(t *testing.T) {
					r := required()
					r.Extensions = map[string]any{"unrelated": map[string]any{"private": "omit"}}
					if seller {
						r.Extensions = buildercode.DeclareBuilderCodeExtension("shop")
						r.Extensions["builder-code"].(map[string]any)["info"].(map[string]any)["s"] = []string{"shop_service", "buyer_service"}
					}
					before, _ := json.Marshal(r)
					var posts, polls atomic.Int32
					var stored map[string]any
					s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						switch request.URL.Path {
						case "/v1/transactions/x402-supported":
							fmt.Fprint(w, `{"kinds":[{"scheme":"balance","network":"inflow:1"}]}`)
						case "/v1/transactions/x402":
							posts.Add(1)
							var body struct {
								Extensions map[string]any `json:"extensions"`
								PaymentID  string         `json:"remotePaymentId"`
							}
							if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
								t.Error(err)
							}
							if body.PaymentID != "pay_12345678901234567890123456789012" {
								t.Error("lost payment identifier")
							}
							stored = body.Extensions
							if seller || service {
								if len(stored) != 1 {
									t.Error("unexpected extensions", stored)
								}
								info := stored["builder-code"].(map[string]any)["info"].(map[string]any)
								if seller && info["a"] != "shop" {
									t.Error("lost seller", info)
								}
								if service {
									want := []any{"buyer_service"}
									if seller {
										want = append(want, "shop_service")
									}
									if !reflect.DeepEqual(info["s"], want) {
										t.Error("incorrect native merge", info)
									}
								}
							} else if len(stored) != 0 {
								t.Error("implicit attribution", stored)
							}
							fmt.Fprint(w, `{"transactionId":"txn","approvalId":"approval"}`)
						case "/v1/transactions/txn/x402":
							polls.Add(1)
							// The fake platform retains submitted metadata; native upstream code,
							// not this handler, performs the seller/buyer merge under test.
							_ = json.NewEncoder(w).Encode(map[string]any{"encodedPayload": "canonical-server-bytes", "paymentPayload": x402.PaymentPayload{X402Version: 2, Accepted: r.Accepts[0], Payload: map[string]any{"proof": "signed"}, Extensions: stored}})
						default:
							t.Error("unexpected request", request.URL.Path)
							w.WriteHeader(500)
						}
					}))
					defer s.Close()
					options := buyer.Options{Options: inflow.Options{BaseURL: s.URL}}
					if service {
						options.BuilderCode = buildercode.NewBuilderCodeClientExtension("buyer_service")
					}
					c, err := buyer.New(options)
					if err != nil {
						t.Fatal(err)
					}
					signOptions := buyer.SignOptions{PaymentID: "pay_12345678901234567890123456789012"}
					check := func(value buyer.EncodedPayment, err error) {
						if err != nil || value.EncodedPayload != "canonical-server-bytes" {
							t.Error(value, err)
						}
						actual, _ := json.Marshal(value.PaymentPayload.Extensions)
						expected, _ := json.Marshal(stored)
						if string(actual) != string(expected) {
							t.Error("changed canonical metadata")
						}
					}
					if prepared {
						p, err := c.Prepare(context.Background(), r, signOptions)
						if err != nil {
							t.Fatal(err)
						}
						var group sync.WaitGroup
						for range 4 {
							group.Go(func() { check(p.Wait(context.Background())) })
						}
						group.Wait()
						check(p.Wait(context.Background()))
					} else {
						check(c.Sign(context.Background(), r, signOptions))
					}
					if posts.Load() != 1 || polls.Load() != 1 {
						t.Fatal("repeated approval or payload retrieval", posts.Load(), polls.Load())
					}
					after, _ := json.Marshal(r)
					if string(before) != string(after) {
						t.Fatal("mutated seller input")
					}
				})
			}
		}
	}
}

func TestAttributionCancellationBeforeApproval(t *testing.T) {
	for _, configured := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		s, posts, _ := server(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected polling") })
		options := buyer.Options{Options: inflow.Options{BaseURL: s.URL}, Hooks: []buyer.Hooks{{Before: func(context.Context, x402.PaymentRequired) error { cancel(); return nil }}}}
		if configured {
			options.BuilderCode = buildercode.NewBuilderCodeClientExtension("buyer")
		}
		c, err := buyer.New(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Prepare(ctx, required(), buyer.SignOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if posts.Load() != 0 {
			t.Fatal("created approval after cancellation")
		}
	}
}
