package buyer_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	fc "github.com/tempoxyz/mpp-go/pkg/client"
)

func TestNativeChallengeOrdering(t *testing.T) {
	for _, mode := range []string{"prefer-second", "exclude-all", "callback-error"} {
		t.Run(mode, func(t *testing.T) {
			var payments, callbacks atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payments.Add(1)
				var input struct{ Challenge mpp.Challenge }
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input.Challenge.ID != "second" {
					t.Error("ordering was ignored", input.Challenge.ID)
				}
				credential, err := mpp.EncodeCredential(mpp.Credential{Challenge: input.Challenge, Payload: map[string]any{"transactionId": transactionID}})
				if err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": credential})
			}))
			defer api.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					w.WriteHeader(204)
					return
				}
				for _, id := range []string{"first", "second"} {
					wire := challenge()
					wire.ID = id
					header, err := mpp.RenderChallenge(wire)
					if err != nil {
						t.Error(err)
					}
					w.Header().Add("WWW-Authenticate", header)
				}
				w.WriteHeader(402)
			}))
			defer resource.Close()
			method, err := newClient(t, api, nil).Method("inflow", "charge", buyer.PaymentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("application declined payment")
			client := fc.New([]fc.Method{method}, fc.WithOrderChallenges(func(ctx context.Context, candidates []fc.ChallengeCandidate) ([]fc.ChallengeCandidate, error) {
				callbacks.Add(1)
				if len(candidates) != 2 {
					t.Fatalf("got %d candidates", len(candidates))
				}
				if mode == "callback-error" {
					return nil, sentinel
				}
				if mode == "exclude-all" {
					return nil, nil
				}
				return candidates[1:], nil
			}))
			request, err := http.NewRequest("GET", resource.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if response != nil {
				response.Body.Close()
			}
			switch mode {
			case "prefer-second":
				if err != nil || response.StatusCode != 204 || payments.Load() != 1 {
					t.Fatalf("payment: %v, count %d", err, payments.Load())
				}
			case "exclude-all":
				if !errors.Is(err, fc.ErrNoMethodFound) || payments.Load() != 0 {
					t.Fatalf("exclusion: %v", err)
				}
			case "callback-error":
				if !errors.Is(err, sentinel) || payments.Load() != 0 {
					t.Fatalf("callback: %v", err)
				}
			}
			if callbacks.Load() != 1 {
				t.Fatal("ordering callback was repeated")
			}
		})
	}
}

func TestNativeTransportSkipsUnsupportedCredentialHeader(t *testing.T) {
	for _, validAlternative := range []bool{false, true} {
		var payments atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payments.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"state": "ready", "credential": credential(t)})
		}))
		resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(204)
				return
			}
			wire := challenge()
			header := "X-Payment"
			wire.Header = &header
			w.Header().Add("WWW-Authenticate", challengeHeader(t, wire))
			if validAlternative {
				w.Header().Add("WWW-Authenticate", challengeHeader(t, challenge()))
			}
			w.WriteHeader(402)
		}))
		method, err := newClient(t, api, nil).Method("inflow", "charge", buyer.PaymentOptions{})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("GET", resource.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := fc.New([]fc.Method{method}).Do(request)
		if response != nil {
			response.Body.Close()
		}
		if validAlternative {
			if err != nil || response.StatusCode != 204 || payments.Load() != 1 {
				t.Fatalf("valid alternative: %v, payments %d", err, payments.Load())
			}
		} else if !errors.Is(err, fc.ErrNoMethodFound) || payments.Load() != 0 {
			t.Fatalf("invalid challenge: %v, payments %d", err, payments.Load())
		}
		resource.Close()
		api.Close()
	}
}
