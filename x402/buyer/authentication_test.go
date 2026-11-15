package buyer_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/extensions/signinwithx"
	foundationhttp "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

type messageSigner struct {
	address string
	sign    func(context.Context, string) (string, error)
}

func (s messageSigner) Address() string { return s.address }
func (s messageSigner) SignMessage(ctx context.Context, message string) (string, error) {
	return s.sign(ctx, message)
}

func TestNativeWalletAuthenticationBeforePayment(t *testing.T) {
	for _, scenario := range []string{"paid", "unpaid", "wrong-origin", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			key, err := crypto.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			var signatures, requests, policyCalls, platformCalls atomic.Int32
			signer := messageSigner{address: crypto.PubkeyToAddress(key.PublicKey).Hex(), sign: func(_ context.Context, message string) (string, error) {
				signatures.Add(1)
				signature, err := crypto.Sign(accounts.TextHash([]byte(message)), key)
				if err != nil {
					return "", err
				}
				signature[64] += 27
				return "0x" + common.Bytes2Hex(signature), nil
			}}
			storage := signinwithx.NewInMemoryStorage()
			if scenario != "unpaid" {
				if err := storage.RecordPayment(context.Background(), "/report", signer.address); err != nil {
					t.Fatal(err)
				}
			}
			var extension *signinwithx.ServerExtension
			challenge := required()
			challenge.Accepts[0].Scheme = "exact"
			challenge.Accepts[0].Network = "eip155:8453"
			merchant := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer application" {
					t.Error("application authentication changed")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "report options" {
					t.Error("body changed")
				}
				if r.Header.Get("Payment-Signature") != "" {
					t.Error("payment sent despite rejecting policy")
				}
				requestContext := foundationhttp.HTTPRequestContext{Adapter: nethttp.NewNetHTTPAdapter(r), Path: "/report", Method: r.Method, Requirements: challenge.Accepts}
				access, err := extension.ProtectedRequestHook()(r.Context(), requestContext, foundationhttp.RouteConfig{Accepts: foundationhttp.PaymentOptions{{Scheme: "exact", Network: "eip155:8453"}}})
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				if access != nil && access.GrantAccess {
					if scenario == "redirect" {
						w.Header().Set("Location", "https://other.example/report")
						w.WriteHeader(307)
						return
					}
					w.WriteHeader(200)
					return
				}
				declaration := signinwithx.DeclareExtension(signinwithx.DeclareOptions{})[signinwithx.ExtensionKey]
				challenge.Extensions = map[string]any{signinwithx.ExtensionKey: extension.EnrichDeclaration(declaration, requestContext)}
				raw, _ := json.Marshal(challenge)
				w.Header().Set("Payment-Required", base64.StdEncoding.EncodeToString(raw))
				w.WriteHeader(402)
			}))
			defer merchant.Close()
			origin := merchant.URL
			if scenario == "wrong-origin" {
				origin = "https://other.example"
			}
			extension, err = signinwithx.CreateResourceServerExtension(signinwithx.ServerOptions{Storage: storage, Origin: origin})
			if err != nil {
				t.Fatal(err)
			}
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				platformCalls.Add(1)
				if r.URL.Path == "/v1/transactions/x402-supported" {
					io.WriteString(w, `{"kinds":[{"scheme":"exact","network":"eip155:8453","x402Version":2}]}`)
					return
				}
				t.Error("authentication or rejected payment created a payment")
				w.WriteHeader(500)
			}))
			defer platform.Close()
			external := foundation.Newx402Client()
			external.RegisterExtension(signinwithx.CreateClientExtension(signer))
			denied := errors.New("payments prohibited")
			client, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: platform.URL, Transport: merchant.Client().Transport}, External: external, Policies: []buyer.Policy{func(context.Context, []foundation.PaymentRequirements) ([]foundation.PaymentRequirements, error) {
				policyCalls.Add(1)
				return nil, denied
			}}})
			if err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequest("POST", merchant.URL+"/report", strings.NewReader("report options"))
			request.Header.Set("Authorization", "Bearer application")
			response, err := client.Do(request, buyer.SignOptions{})
			if scenario == "paid" || scenario == "redirect" {
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				want := 200
				if scenario == "redirect" {
					want = 307
				}
				if response.StatusCode != want || policyCalls.Load() != 0 {
					t.Fatal("authentication did not bypass payment")
				}
			} else if !errors.Is(err, denied) || policyCalls.Load() != 1 {
				t.Fatalf("payment policy bypassed: %v", err)
			}
			wantRequests, wantSignatures := int32(2), int32(1)
			if scenario == "wrong-origin" {
				wantRequests, wantSignatures = 1, 0
			}
			wantPlatform := int32(0)
			if scenario == "unpaid" || scenario == "wrong-origin" {
				wantPlatform = 1
			}
			if requests.Load() != wantRequests || signatures.Load() != wantSignatures || platformCalls.Load() != wantPlatform {
				t.Fatalf("requests=%d signatures=%d platform=%d", requests.Load(), signatures.Load(), platformCalls.Load())
			}
			if request.Header.Get(signinwithx.HeaderName) != "" {
				t.Fatal("caller request mutated")
			}
		})
	}
}
