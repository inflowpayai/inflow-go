package buyer_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
)

func gatewayOffer() x402.PaymentRequirements {
	return x402.PaymentRequirements{Scheme: "exact", Network: "eip155:8453", Asset: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Amount: "1", PayTo: "seller", MaxTimeoutSeconds: 604900, Extra: map[string]any{"name": "GatewayWalletBatched", "version": "1"}}
}

func ordinaryOffer() x402.PaymentRequirements {
	r := gatewayOffer()
	r.MaxTimeoutSeconds = 300
	r.Extra = map[string]any{"name": "USD Coin", "version": "2"}
	return r
}

func gatewayPlatform(t *testing.T, managed bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	creates := new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transactions/x402-supported":
			kinds := []x402.SupportedKind{}
			if managed {
				kinds = append(kinds, x402.SupportedKind{Scheme: "exact", Network: "eip155:8453", X402Version: 2})
			}
			_ = json.NewEncoder(w).Encode(x402.BuyerSupportedResponse{Kinds: kinds})
		case "/v1/transactions/x402":
			creates.Add(1)
			var body struct {
				Accept x402.PaymentRequirements `json:"accept"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body.Accept, ordinaryOffer()) {
				t.Errorf("unexpected payment requirement: %+v, %v", body.Accept, err)
			}
			fmt.Fprint(w, `{"transactionId":"txn","approvalId":"approval","approvalStatus":"APPROVED"}`)
		case "/v1/transactions/txn/x402":
			payload := x402.PaymentPayload{X402Version: 2, Accepted: ordinaryOffer(), Payload: map[string]any{"signature": "synthetic"}}
			data, _ := json.Marshal(payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "SETTLED", "encodedPayload": base64.StdEncoding.EncodeToString(data), "paymentPayload": payload})
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(s.Close)
	return s, creates
}

func assertGatewayError(t *testing.T, err error) {
	t.Helper()
	var failure *buyer.Error
	if !errors.As(err, &failure) || failure.Code != "unsupported-capability" || err.Error() != "Circle Gateway payments are not supported by InFlow." {
		t.Fatalf("expected Gateway rejection, got %v", err)
	}
}

func TestGatewayDirectPreparationAndSigning(t *testing.T) {
	for _, timeout := range []int{300, 3600, 604900} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			s, creates := gatewayPlatform(t, true)
			c := client(t, s)
			r := required()
			r.Accepts = []x402.PaymentRequirements{gatewayOffer()}
			r.Accepts[0].MaxTimeoutSeconds = timeout
			if selected, err := c.Select(context.Background(), r); err != nil || selected != nil {
				t.Fatalf("managed Gateway selection: %+v, %v", selected, err)
			}
			_, err := c.Prepare(context.Background(), r, buyer.SignOptions{})
			assertGatewayError(t, err)
			_, err = c.Sign(context.Background(), r, buyer.SignOptions{})
			assertGatewayError(t, err)
			if creates.Load() != 0 {
				t.Fatal("created a Gateway transaction")
			}
		})
	}
}

func TestGatewayMixedAndExternalRouting(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("managed=%t/mixed=%t", managed, mixed), func(t *testing.T) {
				s, creates := gatewayPlatform(t, managed)
				calls := new(atomic.Int32)
				external := foundation.Newx402Client(foundation.WithSpendControls(foundation.SpendControls{AllowedAssets: []foundation.SpendControlAsset{{Network: "eip155:8453", Asset: gatewayOffer().Asset, MaxAmountPerPayment: "1"}}})).Register("eip155:8453", signer{calls})
				c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, External: external})
				if err != nil {
					t.Fatal(err)
				}
				r := required()
				r.Accepts = []x402.PaymentRequirements{gatewayOffer()}
				if mixed {
					r.Accepts = append(r.Accepts, ordinaryOffer())
				}
				original, _ := json.Marshal(r)
				payment, err := c.Sign(context.Background(), r, buyer.SignOptions{})
				if !mixed {
					assertGatewayError(t, err)
				} else if err != nil || !reflect.DeepEqual(payment.PaymentPayload.Accepted, ordinaryOffer()) {
					t.Fatalf("ordinary offer not paid: %+v, %v", payment, err)
				}
				var wantCreates, wantSigns int32
				if mixed {
					if managed {
						wantCreates = 1
					} else {
						wantSigns = 1
					}
				}
				if creates.Load() != wantCreates || calls.Load() != wantSigns {
					t.Fatalf("creates=%d signs=%d", creates.Load(), calls.Load())
				}
				after, _ := json.Marshal(r)
				if string(original) != string(after) {
					t.Fatal("mutated caller response")
				}
			})
		}
	}
}

func TestGatewayPolicyReplacement(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprint(managed), func(t *testing.T) {
			s, creates := gatewayPlatform(t, managed)
			c, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: s.URL}, External: externalStub{}, Policies: []buyer.Policy{func(context.Context, []x402.PaymentRequirements) ([]x402.PaymentRequirements, error) {
				return []x402.PaymentRequirements{gatewayOffer()}, nil
			}}})
			if err != nil {
				t.Fatal(err)
			}
			r := required()
			r.Accepts = []x402.PaymentRequirements{ordinaryOffer()}
			_, err = c.Sign(context.Background(), r, buyer.SignOptions{})
			if managed {
				var failure *buyer.Error
				if !errors.As(err, &failure) || failure.Code != "unsupported-capability" {
					t.Fatal(err)
				}
			} else {
				assertGatewayError(t, err)
			}
			if creates.Load() != 0 {
				t.Fatal("created a transaction after policy rejection")
			}
		})
	}
}

func TestGatewayDoesNotMaskOtherUnsupportedOffers(t *testing.T) {
	s, _ := gatewayPlatform(t, false)
	c := client(t, s)
	for _, accepts := range [][]x402.PaymentRequirements{nil, {gatewayOffer(), ordinaryOffer()}} {
		r := required()
		r.Accepts = accepts
		_, err := c.Sign(context.Background(), r, buyer.SignOptions{})
		var failure *buyer.Error
		if !errors.As(err, &failure) || failure.Code != "unsupported-capability" || err.Error() != "x402 unsupported-capability" {
			t.Fatal(err)
		}
	}
}

func TestGatewayNameIsExact(t *testing.T) {
	s, _ := gatewayPlatform(t, true)
	c := client(t, s)
	for _, extra := range []map[string]any{nil, {}, {"assetName": "GatewayWalletBatched"}, {"name": "USD Coin"}, {"name": "gatewaywalletbatched"}, {"name": map[string]any{"value": "GatewayWalletBatched"}}} {
		r := required()
		r.Accepts = []x402.PaymentRequirements{ordinaryOffer()}
		r.Accepts[0].Extra = extra
		if selected, err := c.Select(context.Background(), r); err != nil || selected == nil {
			t.Fatalf("metadata rejected: %+v, %v", extra, err)
		}
	}
}

func TestGatewayHTTPDoesNotReplay(t *testing.T) {
	s, creates := gatewayPlatform(t, true)
	c := client(t, s)
	r := required()
	r.Accepts = []x402.PaymentRequirements{gatewayOffer()}
	data, _ := json.Marshal(r)
	var requests atomic.Int32
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(x402.HeaderPaymentSignature) != "" {
			t.Error("Gateway payment sent")
		}
		w.Header().Set(x402.HeaderPaymentRequired, base64.StdEncoding.EncodeToString(data))
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer merchant.Close()
	request, err := http.NewRequest(http.MethodGet, merchant.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(request, buyer.SignOptions{})
	assertGatewayError(t, err)
	if requests.Load() != 1 || creates.Load() != 0 {
		t.Fatalf("requests=%d creates=%d", requests.Load(), creates.Load())
	}
}
