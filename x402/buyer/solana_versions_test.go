package buyer_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	solana "github.com/gagliardetto/solana-go"
	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/extensions/buildercode"
	"github.com/x402-foundation/x402/go/v2/mechanisms/svm"
	svmclient "github.com/x402-foundation/x402/go/v2/mechanisms/svm/exact/client"
)

type solanaTestSigner struct{ key solana.PrivateKey }

func (s solanaTestSigner) Address() solana.PublicKey { return s.key.PublicKey() }
func (s solanaTestSigner) SignTransaction(_ context.Context, tx *solana.Transaction) error {
	_, err := tx.PartialSign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == s.Address() {
			return &s.key
		}
		return nil
	})
	return err
}

func TestNativeSolanaVersionsAndAttribution(t *testing.T) {
	var rpcCalls atomic.Int32
	mint := make([]byte, 82)
	mint[44], mint[45] = 6, 1
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpcCalls.Add(1)
		var request struct {
			ID     json.RawMessage
			Method string
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Method != "getAccountInfo" {
			t.Error("unexpected RPC operation", request.Method)
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"context":{"slot":1234},"value":{"data":["%s","base64"],"executable":false,"lamports":1000000,"owner":"%s","rentEpoch":0}}}`, request.ID, base64.StdEncoding.EncodeToString(mint), solana.TokenProgramID)
	}))
	defer rpc.Close()
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/transactions/x402-supported" {
			t.Error("external signer used managed payment", r.URL.Path)
		}
		fmt.Fprint(w, `{"kinds":[]}`)
	}))
	defer platform.Close()
	network := "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"
	signer := solanaTestSigner{key: solana.NewWallet().PrivateKey}
	for _, tc := range []struct {
		name     string
		versions any
		want     solana.MessageVersion
		rejected bool
	}{
		{"absent", nil, solana.MessageVersionV0, false},
		{"zero", []int{0}, solana.MessageVersionV0, false},
		{"one", []int{1}, solana.MessageVersionV1, false},
		{"both", []int{0, 1}, solana.MessageVersionV1, false},
		{"integral-json", []any{json.Number("0.0"), json.Number("1e0")}, solana.MessageVersionV1, false},
		{"fractional", []any{json.Number("0.5")}, 0, true},
		{"string", []string{"1"}, 0, true},
		{"unsupported", []int{2}, 0, true},
		{"empty", []int{}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			external := foundation.Newx402Client()
			external.Register(foundation.Network(network), svmclient.NewExactSvmScheme(signer, &svm.ClientConfig{RPCURL: rpc.URL}))
			external.RegisterExtension(buildercode.NewBuilderCodeClientExtension("example_client"))
			client, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: platform.URL}, External: external})
			if err != nil {
				t.Fatal(err)
			}
			requirement := x402.PaymentRequirements{Scheme: "exact", Network: network, Amount: "500000", Asset: svm.USDCDevnetAddress, PayTo: solana.NewWallet().PublicKey().String(), MaxTimeoutSeconds: 60,
				Extra: map[string]any{"feePayer": solana.NewWallet().PublicKey().String(), "recentBlockhash": solana.Hash{}.String(), "memo": "local-test"}}
			if tc.versions != nil {
				requirement.Extra["transactionVersions"] = tc.versions
			}
			required := x402.PaymentRequired{X402Version: 2, Accepts: []x402.PaymentRequirements{requirement}, Extensions: buildercode.DeclareBuilderCodeExtension("example_shop")}
			before := rpcCalls.Load()
			result, err := client.Sign(context.Background(), required, buyer.SignOptions{})
			if tc.rejected {
				if err == nil || rpcCalls.Load() != before {
					t.Fatalf("invalid versions reached RPC or signed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			transaction, err := svm.DecodeTransaction(result.PaymentPayload.Payload["transaction"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if transaction.Message.GetVersion() != tc.want {
				t.Fatalf("version %v, want %v", transaction.Message.GetVersion(), tc.want)
			}
			message, err := transaction.Message.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			index, err := transaction.GetAccountIndex(signer.Address())
			if err != nil {
				t.Fatal(err)
			}
			if !transaction.Signatures[index].Verify(signer.Address(), message) {
				t.Fatal("buyer signature is invalid")
			}
			metadata, err := json.Marshal(result.PaymentPayload.Extensions["builder-code"])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(metadata), "example_shop") || !strings.Contains(string(metadata), "example_client") {
				t.Fatal("native attribution was lost", string(metadata))
			}
		})
	}
}

func TestManagedSolanaVersionForwarding(t *testing.T) {
	for _, versions := range [][]int{{0}, {1}, {0, 1}} {
		t.Run(fmt.Sprint(versions), func(t *testing.T) {
			requirement := x402.PaymentRequirements{Scheme: "exact", Network: "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1", Amount: "500000", Asset: svm.USDCDevnetAddress, PayTo: "seller", MaxTimeoutSeconds: 60,
				Extra: map[string]any{"transactionVersions": versions, "opaqueNumber": json.Number("9007199254740993")}}
			want, err := json.Marshal(requirement)
			if err != nil {
				t.Fatal(err)
			}
			var creates atomic.Int32
			platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/transactions/x402-supported":
					json.NewEncoder(w).Encode(x402.BuyerSupportedResponse{Kinds: []x402.SupportedKind{{Scheme: "exact", Network: requirement.Network, X402Version: 2}}})
				case "/v1/transactions/x402":
					creates.Add(1)
					var input struct{ Accept json.RawMessage }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if string(input.Accept) != string(want) {
						t.Errorf("managed requirements changed: %s", input.Accept)
					}
					fmt.Fprint(w, `{"transactionId":"txn","approvalId":"approval","approvalStatus":"APPROVED"}`)
				case "/v1/transactions/txn/x402":
					payload := x402.PaymentPayload{X402Version: 2, Accepted: requirement, Payload: map[string]any{"transaction": "synthetic-managed-payload"}}
					encoded, err := json.Marshal(payload)
					if err != nil {
						t.Error(err)
					}
					json.NewEncoder(w).Encode(map[string]any{"status": "SETTLED", "encodedPayload": base64.StdEncoding.EncodeToString(encoded), "paymentPayload": payload})
				default:
					t.Error("unexpected platform request", r.URL.Path)
				}
			}))
			defer platform.Close()
			client, err := buyer.New(buyer.Options{Options: inflow.Options{BaseURL: platform.URL}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Sign(context.Background(), x402.PaymentRequired{X402Version: 2, Resource: &x402.ResourceInfo{URL: "https://seller.example/data"}, Accepts: []x402.PaymentRequirements{requirement}}, buyer.SignOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(result.PaymentPayload.Accepted)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) || creates.Load() != 1 {
				t.Fatalf("managed version metadata changed: %s", got)
			}
		})
	}
}
