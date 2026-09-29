package seller_test

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
	"sync"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

func sameJSON(t *testing.T, a, b []byte) {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(x, y) {
		t.Errorf("got %s; want %s", a, b)
	}
}

func offerFrom(t *testing.T, method, intent string, raw json.RawMessage) seller.Offer {
	t.Helper()
	var offer seller.Offer
	var target any
	if method == "tempo" {
		offer.Tempo = &mpp.TempoRequest{}
		target = offer.Tempo
	} else if intent == "subscription" {
		offer.Subscription = &mpp.SubscriptionRequest{}
		target = offer.Subscription
	} else {
		offer.Charge = &mpp.ChargeRequest{}
		target = offer.Charge
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
	return offer
}

func clientFor(t *testing.T, server *httptest.Server) *seller.Client {
	t.Helper()
	c, err := seller.New(inflow.Options{BaseURL: server.URL, APIKey: "test-only-seller-key"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSharedSellerCases(t *testing.T) {
	data, err := os.ReadFile("testdata/seller.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Operation string
		Input         struct {
			Method, Intent string
			Request        json.RawMessage
			Replacement    json.RawMessage `json:"replacement_request"`
			Credential     mpp.Credential
			Payload        map[string]any `json:"credential_payload"`
			Source         *string
		}
		Expect struct {
			Result json.RawMessage
			Error  *struct {
				Code    string
				Details struct{ Problem json.RawMessage }
			}
		}
		Platform struct {
			Exchanges []struct {
				Request struct {
					Method, Path string
					Headers      map[string]json.RawMessage
					JSON         json.RawMessage
				}
				Response struct {
					Status int
					JSON   json.RawMessage
				}
			}
		}
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 40 {
		t.Fatal(len(cases))
	}
	for _, test := range cases {
		t.Run(test.ID, func(t *testing.T) {
			var mu sync.Mutex
			index := 0
			captured := map[string]string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if index >= len(test.Platform.Exchanges) {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				e := test.Platform.Exchanges[index]
				index++
				if r.Method != e.Request.Method || r.URL.Path != e.Request.Path {
					t.Errorf("got %s %s; want %s %s", r.Method, r.URL.Path, e.Request.Method, e.Request.Path)
				}
				for key, value := range e.Request.Headers {
					var literal string
					if string(value) == "null" {
						if r.Header.Get(key) != "" {
							t.Errorf("unexpected %s", key)
						}
						continue
					}
					if json.Unmarshal(value, &literal) == nil {
						if r.Header.Get(key) != literal {
							t.Errorf("header %s mismatch", key)
						}
						continue
					}
					var rule struct {
						Capture, Same string
						Pattern       string
					}
					if err := json.Unmarshal(value, &rule); err != nil {
						t.Error(err)
					}
					if r.Header.Get(key) == "" {
						t.Errorf("missing %s", key)
					}
					if rule.Capture != "" {
						captured[rule.Capture] = r.Header.Get(key)
					}
					if rule.Same != "" && r.Header.Get(key) != captured[rule.Same] {
						t.Errorf("changed %s", key)
					}
				}
				body, _ := io.ReadAll(r.Body)
				if len(e.Request.JSON) > 0 {
					sameJSON(t, body, e.Request.JSON)
				}
				w.WriteHeader(e.Response.Status)
				w.Write(e.Response.JSON)
			}))
			defer server.Close()
			c := clientFor(t, server)
			var result any
			var err error
			switch test.Operation {
			case "mpp.seller.prepare":
				var p seller.PreparedOffer
				p, err = c.Prepare(context.Background(), offerFrom(t, test.Input.Method, test.Input.Intent, test.Input.Request))
				if err == nil {
					result, err = mpp.Decode(p.Request)
				}
			case "mpp.seller.validate":
				result, err = c.Validate(context.Background(), test.Input.Credential)
			case "mpp.seller.verify":
				result, err = c.Verify(context.Background(), test.Input.Credential)
			case "mpp.seller.route-binding":
				route := seller.Route{Realm: "seller.example", SecretKey: "test-only-binding-key", Offers: []seller.Offer{offerFrom(t, test.Input.Method, test.Input.Intent, test.Input.Request)}}
				h, e := c.Protect(route, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") }))
				if e != nil {
					t.Fatal(e)
				}
				first := httptest.NewRecorder()
				h.ServeHTTP(first, httptest.NewRequest("GET", "http://seller.example/paid", nil))
				challenges, e := mpp.ParseChallenges(first.Header().Values("WWW-Authenticate"))
				if e != nil || len(challenges) != 1 {
					t.Fatal(first.Code, e)
				}
				encoded, e := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: test.Input.Payload, Source: test.Input.Source})
				if e != nil {
					t.Fatal(e)
				}
				route.Offers = []seller.Offer{offerFrom(t, test.Input.Method, test.Input.Intent, test.Input.Replacement)}
				h, e = c.Protect(route, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("altered route accepted") }))
				if e != nil {
					t.Fatal(e)
				}
				r := httptest.NewRequest("GET", "http://seller.example/paid", nil)
				r.Header.Set("Authorization", "Payment "+encoded)
				second := httptest.NewRecorder()
				h.ServeHTTP(second, r)
				result = map[string]int{"status": second.Code}
			default:
				t.Fatal(test.Operation)
			}
			if test.Expect.Error != nil {
				var problem *seller.Error
				if !errors.As(err, &problem) {
					t.Fatalf("expected Seller failure; got %v", err)
				}
				code := problem.Code
				if code == "ambiguous-rail" || code == "instrument-required" {
					code = "unsupported-capability"
				}
				if code != test.Expect.Error.Code {
					t.Fatalf("code %s", code)
				}
				if len(test.Expect.Error.Details.Problem) > 0 {
					sameJSON(t, problem.Problem, test.Expect.Error.Details.Problem)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				encoded, e := json.Marshal(result)
				if e != nil {
					t.Fatal(e)
				}
				sameJSON(t, encoded, test.Expect.Result)
			}
			mu.Lock()
			defer mu.Unlock()
			if index != len(test.Platform.Exchanges) {
				t.Fatalf("used %d of %d requests", index, len(test.Platform.Exchanges))
			}
		})
	}
}

const configJSON = `{"sellerId":"11111111-1111-4111-8111-111111111111","featureFlags":{"idempotencyKeyEnabled":true},"supportedMethods":[{"id":"inflow","methodDetails":{"intentCurrencyRails":{"charge":{"USDC":[{"rail":"balance"}]},"subscription":{"USDC":[{"rail":"balance"}]}}}}]}`

func chargeOffer() seller.Offer {
	return seller.Offer{Charge: &mpp.ChargeRequest{Amount: "1", Currency: "USDC"}}
}

func writeValidation(w http.ResponseWriter, r *http.Request) {
	var input struct{ Credential mpp.Credential }
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.Decode(&input)
	request, _ := mpp.Decode(input.Credential.Challenge.Request)
	json.NewEncoder(w).Encode(seller.Validation{Success: true, Challenge: input.Credential.Challenge, Credential: input.Credential, Details: map[string]json.RawMessage{}, Intent: input.Credential.Challenge.Intent, Method: input.Credential.Challenge.Method, Request: rawMap(request), Source: input.Credential.Source})
}

func rawMap(v any) map[string]json.RawMessage {
	b, _ := json.Marshal(v)
	var result map[string]json.RawMessage
	json.Unmarshal(b, &result)
	return result
}

func receiptJSON() string {
	return `{"receipt":{"method":"inflow","reference":"test","status":"success","timestamp":"2026-09-28T00:00:00Z"}}`
}

func TestHTTPRoundTrip(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("X-API-Key") != "test-only-seller-key" {
			t.Error("missing Seller key")
		}
		switch r.URL.Path {
		case "/v1/mpp/config":
			fmt.Fprint(w, configJSON)
		case "/v1/mpp/validate":
			writeValidation(w, r)
		case "/v1/mpp/broadcast":
			fmt.Fprint(w, receiptJSON())
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer platform.Close()
	c := clientFor(t, platform)
	offer := chargeOffer()
	h, err := c.Protect(seller.Route{Realm: "seller.example", SecretKey: "test-only-key", Offers: []seller.Offer{offer}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "paid") }))
	if err != nil {
		t.Fatal(err)
	}
	offer.Charge.Amount = "999"
	resource := httptest.NewServer(h)
	defer resource.Close()
	response, err := http.Get(resource.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	challenges, err := mpp.ParseChallenges(response.Header.Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 {
		t.Fatal(err)
	}
	request, _ := mpp.Decode(challenges[0].Request)
	if request.(map[string]any)["amount"] != "1" {
		t.Fatal("caller mutation changed route")
	}
	encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"transactionId": "test", "large": json.Number("9007199254740993")}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", resource.URL, nil)
	r.Header.Set("Authorization", "Payment "+encoded)
	response, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "paid" || response.Header.Get("Payment-Receipt") == "" {
		t.Fatal(response.Status, string(body))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatal(calls)
	}
}

func TestBuyerSellerHTTPFlows(t *testing.T) {
	for _, kind := range []string{"charge", "subscription", "tempo"} {
		t.Run(kind, func(t *testing.T) {
			var paid, handled int
			var mu sync.Mutex
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/mpp/config":
					fmt.Fprint(w, configJSON)
				case "/v1/transactions/mpp":
					if r.Header.Get("X-API-Key") != "test-buyer" {
						t.Error("wrong buyer key")
					}
					var input struct{ Challenge mpp.Challenge }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					payload := map[string]any{"transactionId": "test"}
					if kind == "tempo" {
						payload["type"] = "transaction"
						payload["signature"] = "0xabcd"
					}
					encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: payload})
					if err != nil {
						t.Error(err)
					}
					fmt.Fprintf(w, `{"state":"ready","credential":%q}`, encoded)
				case "/v1/mpp/validate":
					writeValidation(w, r)
				case "/v1/mpp/broadcast":
					mu.Lock()
					paid++
					mu.Unlock()
					fmt.Fprint(w, receiptJSON())
				default:
					t.Error(r.URL.Path)
				}
			}))
			defer platform.Close()
			c := clientFor(t, platform)
			offer := chargeOffer()
			if kind == "subscription" {
				offer = seller.Offer{Subscription: &mpp.SubscriptionRequest{ChargeRequest: *chargeOffer().Charge, PeriodUnit: "month", PeriodCount: 1, SubscriptionExpires: "2099-01-01T00:00:00Z"}}
			}
			if kind == "tempo" {
				offer = seller.Offer{Tempo: &mpp.TempoRequest{Amount: "10", Currency: "0x1111111111111111111111111111111111111111", Recipient: "0x2222222222222222222222222222222222222222"}}
			}
			opaque := "eyJyb3V0ZSI6InBhaWQifQ"
			var advertised bool
			h, err := c.Protect(seller.Route{Realm: "seller.example", SecretKey: "test-key", Offers: []seller.Offer{offer}, Opaque: &opaque, CanOffer: func(*http.Request, seller.Offer) (bool, error) {
				mu.Lock()
				defer mu.Unlock()
				if advertised {
					return false, nil
				}
				advertised = true
				return true, nil
			}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if paid != 1 {
					t.Error("handler before payment")
				}
				handled++
				if r.Header.Get("X-App-Key") != "application-key" {
					t.Error("application authentication lost")
				}
				fmt.Fprint(w, "result")
			}))
			if err != nil {
				t.Fatal(err)
			}
			resource := httptest.NewServer(h)
			defer resource.Close()
			b, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: platform.URL, APIKey: "test-buyer"}})
			if err != nil {
				t.Fatal(err)
			}
			r, _ := http.NewRequest("GET", resource.URL, nil)
			r.Header.Set("X-App-Key", "application-key")
			response, err := b.Do(r, buyer.PaymentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 || string(body) != "result" {
				t.Fatal(response.Status, string(body))
			}
			mu.Lock()
			defer mu.Unlock()
			if handled != 1 || paid != 1 {
				t.Fatal(handled, paid)
			}
		})
	}
}
