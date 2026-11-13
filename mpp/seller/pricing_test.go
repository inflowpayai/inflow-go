package seller_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
	upstream "github.com/tempoxyz/mpp-go/pkg/mpp"
)

func TestRequestPricingLifecycle(t *testing.T) {
	for _, bodyAware := range []bool{false, true} {
		t.Run(fmt.Sprint(bodyAware), func(t *testing.T) {
			var validations, broadcasts, handled, resolved atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/mpp/config":
					fmt.Fprint(w, configJSON)
				case "/v1/mpp/validate":
					validations.Add(1)
					var input struct{ Credential mpp.Credential }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					request, _ := mpp.Decode(input.Credential.Challenge.Request)
					json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": input.Credential.Challenge, "credential": input.Credential, "request": request, "source": input.Credential.Source, "method": "inflow", "intent": "charge"})
				case "/v1/mpp/broadcast":
					broadcasts.Add(1)
					fmt.Fprint(w, `{"receipt":{"method":"inflow","reference":"test","status":"success","timestamp":"2026-09-28T00:00:00Z"}}`)
				default:
					t.Error(r.URL.Path)
				}
			}))
			defer api.Close()
			options := inflow.RequestPricingOptions{}
			if bodyAware {
				options.MaxBodyBytes = 8
			}
			handler, err := clientFor(t, api).ProtectRequest(options, func(r *http.Request, body []byte) (seller.Route, error) {
				resolved.Add(1)
				amount := r.URL.Query().Get("price")
				if bodyAware && string(body) == "large" {
					amount = "2"
				}
				return seller.Route{Realm: "seller.example", SecretKey: "test", Offers: []seller.Offer{{Charge: &mpp.ChargeRequest{Amount: amount, Currency: "USDC"}}}}, nil
			}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handled.Add(1)
				io.Copy(w, r.Body)
			}))
			if err != nil {
				t.Fatal(err)
			}
			call := func(price, body, credential string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/paid?price="+price, strings.NewReader(body))
				if credential != "" {
					r.Header.Set("Authorization", "Payment "+credential)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			unpaid := call("1", "small", "")
			challenge := pricingChallenge(t, unpaid)
			if bodyAware {
				if challenge.Digest == nil || !upstream.VerifyBodyDigest(*challenge.Digest, []byte("small")) {
					t.Fatal("missing body binding")
				}
			} else if challenge.Digest != nil {
				t.Fatal("metadata request bound body")
			}
			credential, _ := mpp.EncodeCredential(mpp.Credential{Challenge: challenge, Payload: map[string]any{"transactionId": "test"}})
			changed := call("2", "large", credential)
			if changed.Code != 402 || handled.Load() != 0 || validations.Load() != 0 {
				t.Fatal("stale price accepted", changed)
			}
			if bodyAware {
				changedBody := call("1", "other", credential)
				if changedBody.Code != 402 || validations.Load() != 0 {
					t.Fatal("changed body accepted", changedBody)
				}
			}
			paid := call("1", "small", credential)
			if paid.Code != 200 || paid.Body.String() != "small" || paid.Header().Get("Payment-Receipt") == "" || handled.Load() != 1 || validations.Load() != 1 || broadcasts.Load() != 1 {
				t.Fatal(paid, handled.Load(), validations.Load(), broadcasts.Load())
			}
			before := resolved.Load()
			var group sync.WaitGroup
			for i := range 20 {
				group.Go(func() {
					price := fmt.Sprint(i + 1)
					response := call(price, "small", "")
					c := pricingChallenge(t, response)
					value, _ := mpp.Decode(c.Request)
					if value.(map[string]any)["amount"] != price {
						t.Errorf("cross-request price: %v", value)
					}
				})
			}
			group.Wait()
			if resolved.Load()-before != 20 {
				t.Fatal("resolver not once per request")
			}
		})
	}
}

func pricingChallenge(t *testing.T, response *httptest.ResponseRecorder) mpp.Challenge {
	t.Helper()
	challenges, err := mpp.ParseChallenges(response.Header().Values("WWW-Authenticate"))
	if response.Code != 402 || err != nil || len(challenges) != 1 {
		t.Fatalf("challenge: %d %s %v", response.Code, response.Body, err)
	}
	return challenges[0]
}

func TestRequestPricingErrors(t *testing.T) {
	client, _ := seller.New(inflow.Options{APIKey: "key"})
	for _, mode := range []string{"resolver", "route"} {
		handler, err := client.ProtectRequest(inflow.RequestPricingOptions{}, func(*http.Request, []byte) (seller.Route, error) {
			if mode == "resolver" {
				return seller.Route{}, errors.New("private")
			}
			return seller.Route{}, nil
		}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }))
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
		if response.Code != 500 || response.Header().Get("WWW-Authenticate") != "" || strings.Contains(response.Body.String(), "private") {
			t.Fatal(response)
		}
	}
	if _, err := client.ProtectRequest(inflow.RequestPricingOptions{}, nil, nil); err == nil {
		t.Fatal("accepted nil handler")
	}
}

func TestRequestPricingPaymentFailure(t *testing.T) {
	for _, mode := range []string{"validation-http", "broadcast-http", "broadcast-empty"} {
		t.Run(mode, func(t *testing.T) {
			var broadcasts atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/mpp/config":
					fmt.Fprint(w, strings.Replace(configJSON, "true", "false", 1))
				case "/v1/mpp/validate":
					if mode == "validation-http" {
						w.WriteHeader(400)
						return
					}
					var input struct{ Credential mpp.Credential }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					request, _ := mpp.Decode(input.Credential.Challenge.Request)
					json.NewEncoder(w).Encode(map[string]any{"success": true, "challenge": input.Credential.Challenge, "credential": input.Credential, "request": request, "source": input.Credential.Source, "method": "inflow", "intent": "charge"})
				case "/v1/mpp/broadcast":
					broadcasts.Add(1)
					if mode == "broadcast-http" {
						w.WriteHeader(503)
					}
				default:
					t.Error(r.URL.Path)
				}
			}))
			defer api.Close()
			handler, err := clientFor(t, api).ProtectRequest(inflow.RequestPricingOptions{}, func(*http.Request, []byte) (seller.Route, error) {
				return seller.Route{Realm: "seller.example", SecretKey: "test", Offers: []seller.Offer{chargeOffer()}}, nil
			}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }))
			if err != nil {
				t.Fatal(err)
			}
			unpaid := httptest.NewRecorder()
			handler.ServeHTTP(unpaid, httptest.NewRequest("GET", "/paid", nil))
			challenge := pricingChallenge(t, unpaid)
			credential, _ := mpp.EncodeCredential(mpp.Credential{Challenge: challenge, Payload: map[string]any{"transactionId": "test"}})
			request := httptest.NewRequest("GET", "/paid", nil)
			request.Header.Set("Authorization", "Payment "+credential)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := int32(1)
			if mode == "validation-http" {
				want = 0
			}
			if response.Code != 502 || response.Header().Get("WWW-Authenticate") != "" || response.Header().Get("Payment-Receipt") != "" || broadcasts.Load() != want {
				t.Fatal(response, broadcasts.Load())
			}
		})
	}
}
