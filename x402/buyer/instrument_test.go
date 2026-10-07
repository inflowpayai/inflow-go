package buyer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

func TestInstrumentSelection(t *testing.T) {
	for _, test := range []struct {
		scheme, id, extension, want string
		rejected                    bool
	}{
		{"instrument", "owned-card", "", "owned-card", false},
		{"instrument", "", "", "", false},
		{"instrument", "selected", "extension", "selected", false},
		{"balance", "selected", "", "", false},
		{"exact", "selected", "", "", false},
		{"instrument", "unavailable", "", "unavailable", true},
	} {
		t.Run(test.scheme+"/"+test.id+"/"+test.extension, func(t *testing.T) {
			creates := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/transactions/x402-supported":
					fmt.Fprintf(w, `{"kinds":[{"scheme":%q,"network":"inflow:1","x402Version":2}]}`, test.scheme)
				case "/v1/transactions/x402":
					creates++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					id, exists := body["instrumentId"]
					if test.want == "" && exists || test.want != "" && id != test.want {
						t.Errorf("instrumentId: %v", id)
					}
					if test.rejected {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"errors":[{"code":"INVALID_INSTRUMENT","message":"Unavailable"}]}`)
						return
					}
					fmt.Fprint(w, `{"transactionId":"tx","approvalId":"approval","approvalStatus":"PENDING"}`)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			defer s.Close()
			c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, InstrumentID: test.id})
			if err != nil {
				t.Fatal(err)
			}
			req := required()
			req.Accepts[0].Scheme = test.scheme
			extensions := map[string]any{"sentinel": "untouched"}
			if test.extension != "" {
				extensions["instrumentId"] = test.extension
			}
			before := map[string]any{}
			for k, v := range extensions {
				before[k] = v
			}
			_, err = c.Prepare(context.Background(), req, buyer.SignOptions{TransactionRequestExtensions: extensions})
			if (err != nil) != test.rejected {
				t.Fatalf("error: %v", err)
			}
			if creates != 1 {
				t.Fatalf("created %d times", creates)
			}
			if !reflect.DeepEqual(before, extensions) {
				t.Fatal("mutated caller extensions")
			}
		})
	}
}
