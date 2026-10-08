package seller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

func TestInstrumentReceiptBinding(t *testing.T) {
	for _, test := range []struct {
		name, rail, method string
		challenge          *string
		invalid            bool
	}{
		{"matching", "instrument", "inflow", new("test"), false},
		{"missing", "instrument", "inflow", nil, true},
		{"empty", "instrument", "inflow", new(""), true},
		{"wrong-challenge", "instrument", "inflow", new("other"), true},
		{"wrong-method", "instrument", "tempo", new("test"), true},
		{"balance", "balance", "inflow", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			broadcasts := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/mpp/config" {
					fmt.Fprint(w, configJSON)
					return
				}
				if r.URL.Path != "/v1/mpp/broadcast" {
					t.Errorf("unexpected path %s", r.URL)
					w.WriteHeader(500)
					return
				}
				broadcasts++
				json.NewEncoder(w).Encode(map[string]any{"receipt": mpp.Receipt{Method: test.method, Reference: "receipt", Status: "success", Timestamp: "2026-10-07T00:00:00Z", ChallengeID: test.challenge}})
			}))
			defer s.Close()
			credential := sampleCredential()
			var err error
			credential.Challenge.Request, err = mpp.Encode(map[string]any{"methodDetails": map[string]any{"rail": test.rail}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = clientFor(t, s).Broadcast(context.Background(), credential, "")
			if (err != nil) != test.invalid {
				t.Fatalf("error: %v", err)
			}
			if broadcasts != 1 {
				t.Fatalf("broadcast %d times", broadcasts)
			}
		})
	}
}

func TestBroadcastRejectsMalformedRequestBeforeNetwork(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected network request")
		w.WriteHeader(500)
	}))
	defer s.Close()
	for _, request := range []string{"invalid", "W10"} {
		credential := sampleCredential()
		credential.Challenge.Request = request
		if _, err := clientFor(t, s).Broadcast(context.Background(), credential, ""); err == nil {
			t.Fatal("accepted malformed request")
		}
	}
}

func TestInstrumentProtectedRoute(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(fmt.Sprint(matching), func(t *testing.T) {
			broadcasts, served := 0, 0
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/mpp/config":
					fmt.Fprint(w, strings.ReplaceAll(strings.ReplaceAll(configJSON, "USDC", "USD"), "balance", "instrument"))
				case "/v1/mpp/validate":
					writeValidation(w, r)
				case "/v1/mpp/broadcast":
					broadcasts++
					var body struct{ Credential mpp.Credential }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					id := "wrong"
					if matching {
						id = body.Credential.Challenge.ID
					}
					json.NewEncoder(w).Encode(map[string]any{"receipt": mpp.Receipt{Method: "inflow", Reference: "receipt", Status: "success", Timestamp: "2026-10-07T00:00:00Z", ChallengeID: &id}})
				default:
					t.Errorf("unexpected path %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			defer platform.Close()
			offer := chargeOffer()
			offer.Charge.Currency = "USD"
			offer.Charge.MethodDetails = &mpp.InflowMethodDetails{Rail: "instrument"}
			handler, err := clientFor(t, platform).Protect(seller.Route{Realm: "seller.example", SecretKey: "test-only-key", Offers: []seller.Offer{offer}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++; w.WriteHeader(200) }))
			if err != nil {
				t.Fatal(err)
			}
			challengeResponse := httptest.NewRecorder()
			handler.ServeHTTP(challengeResponse, httptest.NewRequest("GET", "https://seller.example/paid", nil))
			challenges, err := mpp.ParseChallenges(challengeResponse.Header().Values("WWW-Authenticate"))
			if err != nil || len(challenges) != 1 {
				t.Fatalf("challenge: %v %v", challenges, err)
			}
			encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"type": "instrument", "transactionId": "tx"}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "https://seller.example/paid", nil)
			req.Header.Set("Authorization", "Payment "+encoded)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if broadcasts != 1 {
				t.Fatalf("broadcasts: %d", broadcasts)
			}
			if matching {
				if served != 1 || response.Code != 200 || response.Header().Get("Payment-Receipt") == "" {
					t.Fatalf("paid response: %d %d", response.Code, served)
				}
			} else if served != 0 || response.Code == 200 || response.Header().Get("Payment-Receipt") != "" {
				t.Fatalf("released mismatched payment: %d %d", response.Code, served)
			}
		})
	}
}
