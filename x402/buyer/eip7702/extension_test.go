package eip7702

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/inflowpayai/inflow-go/x402"
	foundation "github.com/x402-foundation/x402/go/v2"
)

var _ foundation.ClientExtension = (*Extension)(nil)

type fixture struct {
	Owner    string              `json:"owner"`
	Payload  x402.PaymentPayload `json:"payload"`
	Prepared map[string]any      `json:"prepared"`
}

func load(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/node.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

type wallet struct {
	key                       *ecdsa.PrivateKey
	allowance                 any
	readErr, signErr, authErr error
	mutate                    func(*types.SetCodeAuthorization)
	message                   func([]byte) ([]byte, error)
	reads, signs, auths       int
}

func newWallet(t *testing.T) *wallet {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	return &wallet{key: key, allowance: new(big.Int)}
}
func (w *wallet) Address() string { return crypto.PubkeyToAddress(w.key.PublicKey).Hex() }
func (w *wallet) ReadContract(_ context.Context, address string, definition []byte, name string, args ...any) (any, error) {
	w.reads++
	return w.allowance, w.readErr
}
func (w *wallet) SignMessage(_ context.Context, message []byte) ([]byte, error) {
	w.signs++
	if w.message != nil {
		return w.message(message)
	}
	if w.signErr != nil {
		return nil, w.signErr
	}
	return crypto.Sign(accounts.TextHash(message), w.key)
}
func (w *wallet) SignAuthorization(_ context.Context, a types.SetCodeAuthorization) (types.SetCodeAuthorization, error) {
	w.auths++
	if w.authErr != nil {
		return a, w.authErr
	}
	if w.mutate != nil {
		w.mutate(&a)
	}
	return types.SignSetCode(w.key, a)
}

func extension(t *testing.T, f fixture, w *wallet, consent func(context.Context, types.SetCodeAuthorization) (bool, error)) (*Extension, *atomic.Int32) {
	t.Helper()
	requests := new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/x402/eip7702/prepare" || r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" {
			t.Error("wrong preparation request")
		}
		var body struct {
			PaymentPayload      x402.PaymentPayload      `json:"paymentPayload"`
			PaymentRequirements x402.PaymentRequirements `json:"paymentRequirements"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.PaymentRequirements, f.Payload.Accepted) {
			t.Error("wrong payment binding")
		}
		_ = json.NewEncoder(out).Encode(f.Prepared)
	}))
	t.Cleanup(s.Close)
	if consent == nil {
		consent = func(context.Context, types.SetCodeAuthorization) (bool, error) { return true, nil }
	}
	e, err := New(Options{BaseURL: s.URL, Signer: w, Consent: consent})
	if err != nil {
		t.Fatal(err)
	}
	return e, requests
}

func TestNodeGoldenAndRealSignatures(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "already delegated", true: "consent"}[authorized], func(t *testing.T) {
			f := load(t)
			if !authorized {
				delete(f.Prepared, "authorization")
			}
			w := newWallet(t)
			w.message = func(b []byte) ([]byte, error) {
				signature, err := crypto.Sign(accounts.TextHash(b), w.key)
				if err == nil {
					signature[64] += 27
				}
				return signature, err
			}
			consents := 0
			e, requests := extension(t, f, w, func(context.Context, types.SetCodeAuthorization) (bool, error) { consents++; return true, nil })
			before, _ := json.Marshal(f.Payload)
			value, err := e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()})
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(f.Payload)
			if string(before) != string(after) {
				t.Fatal("modified caller payment")
			}
			info := value.Extensions[e.Key()].(map[string]any)["info"].(x402.SponsorshipInfo)
			sig, ok := hexBytes(info.Signature)
			if !ok || len(sig) != 65 {
				t.Fatal("invalid signature")
			}
			hash, _ := hexBytes(f.Prepared["userOperationHash"].(string))
			sig[64] -= 27
			pub, err := crypto.SigToPub(accounts.TextHash(hash), sig)
			if err != nil || crypto.PubkeyToAddress(*pub) != common.HexToAddress(f.Owner) {
				t.Fatal("wrong operation signer")
			}
			if requests.Load() != 1 || w.signs != 1 || w.reads != 1 {
				t.Fatal("unexpected repeated operation")
			}
			if authorized && (consents != 1 || w.auths != 1 || info.AuthorizationSignature == "") {
				t.Fatal("missing authorization")
			}
			if !authorized && (consents != 0 || w.auths != 0 || info.AuthorizationSignature != "") {
				t.Fatal("unnecessary delegation")
			}
		})
	}
}

func TestPreparedOperationTampering(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"chain":                func(p map[string]any) { p["chainId"] = 1 },
		"identifier":           func(p map[string]any) { p["sponsorshipId"] = "invalid" },
		"EntryPoint":           func(p map[string]any) { p["entryPoint"] = "0x" + strings.Repeat("11", 20) },
		"version":              func(p map[string]any) { p["entryPointVersion"] = "0.8" },
		"delegation":           func(p map[string]any) { p["delegation"] = "invalid" },
		"hash":                 func(p map[string]any) { p["userOperationHash"] = "0x" + strings.Repeat("00", 32) },
		"expired":              func(p map[string]any) { p["expiresAt"] = 1 },
		"beyond payment":       func(p map[string]any) { p["expiresAt"] = 4102444900 },
		"null authorization":   func(p map[string]any) { p["authorization"] = nil },
		"missing nonce":        func(p map[string]any) { delete(p["authorization"].(map[string]any), "nonce") },
		"authorization extra":  func(p map[string]any) { p["authorization"].(map[string]any)["extra"] = true },
		"authorization chain":  func(p map[string]any) { p["authorization"].(map[string]any)["chainId"] = 1 },
		"authorization target": func(p map[string]any) { p["authorization"].(map[string]any)["address"] = "invalid" },
	}
	for _, field := range []string{"sender", "nonce", "callData", "callGasLimit", "verificationGasLimit", "preVerificationGas", "maxFeePerGas", "maxPriorityFeePerGas", "paymaster", "paymasterData", "paymasterVerificationGasLimit", "paymasterPostOpGasLimit", "factory"} {
		mutations[field] = func(p map[string]any) { p["userOperation"].(map[string]any)[field] = "0x1" }
	}
	// Nonzero gas changes need a hash check; zero gas exercises the independent profile constraint.
	mutations["callGasLimit"] = func(p map[string]any) { p["userOperation"].(map[string]any)["callGasLimit"] = "0x0" }
	mutations["verificationGasLimit"] = func(p map[string]any) { p["userOperation"].(map[string]any)["verificationGasLimit"] = "0x0" }
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := load(t)
			mutate(f.Prepared)
			w := newWallet(t)
			e, _ := extension(t, f, w, nil)
			if _, err := e.EnrichPaymentPayload(context.Background(), f.Payload, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()}); err == nil {
				t.Fatal("accepted tampered operation")
			}
			if w.signs != 0 || w.auths != 0 {
				t.Fatal("signed tampered operation")
			}
		})
	}
}

func TestConsentAndSignerFailures(t *testing.T) {
	sentinel := errors.New("wallet failure")
	for _, mode := range []string{"decline", "consent error", "read error", "bad allowance", "negative allowance", "authorization error", "changed authorization", "wrong authorization signer", "sign error", "short signature", "wrong signature", "cancel in consent", "cancel after authorization", "cancel after signature"} {
		t.Run(mode, func(t *testing.T) {
			f := load(t)
			w := newWallet(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			consent := func(context.Context, types.SetCodeAuthorization) (bool, error) { return true, nil }
			switch mode {
			case "decline":
				consent = func(context.Context, types.SetCodeAuthorization) (bool, error) { return false, nil }
			case "consent error":
				consent = func(context.Context, types.SetCodeAuthorization) (bool, error) { return false, sentinel }
			case "read error":
				w.readErr = sentinel
			case "bad allowance":
				w.allowance = "0"
			case "negative allowance":
				w.allowance = big.NewInt(-1)
			case "authorization error":
				w.authErr = sentinel
			case "changed authorization":
				w.mutate = func(a *types.SetCodeAuthorization) { a.Nonce++ }
			case "wrong authorization signer":
				w.mutate = func(a *types.SetCodeAuthorization) { w.key, _ = crypto.HexToECDSA(strings.Repeat("02", 32)) }
			case "sign error":
				w.signErr = sentinel
			case "short signature":
				w.message = func([]byte) ([]byte, error) { return nil, nil }
			case "wrong signature":
				w.message = func(b []byte) ([]byte, error) {
					other, _ := crypto.HexToECDSA(strings.Repeat("02", 32))
					return crypto.Sign(accounts.TextHash(b), other)
				}
			case "cancel in consent":
				consent = func(context.Context, types.SetCodeAuthorization) (bool, error) { cancel(); return true, nil }
			case "cancel after authorization":
				w.mutate = func(*types.SetCodeAuthorization) { cancel() }
			case "cancel after signature":
				w.message = func(b []byte) ([]byte, error) { cancel(); return crypto.Sign(accounts.TextHash(b), w.key) }
			}
			e, _ := extension(t, f, w, consent)
			if _, err := e.EnrichPaymentPayload(ctx, f.Payload, x402.PaymentRequired{Extensions: x402.DeclareSponsorship()}); err == nil {
				t.Fatal("ignored rejection")
			}
		})
	}
}
