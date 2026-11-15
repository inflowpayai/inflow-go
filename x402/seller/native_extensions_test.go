package seller

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	inflow "github.com/inflowpayai/inflow-go"
	foundation "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/extensions/bazaar"
	"github.com/x402-foundation/x402/go/v2/extensions/buildercode"
	"github.com/x402-foundation/x402/go/v2/extensions/signinwithx"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
)

type extensionSigner struct{ key *ecdsa.PrivateKey }

func (s extensionSigner) Address() string { return crypto.PubkeyToAddress(s.key.PublicKey).Hex() }
func (s extensionSigner) SignMessage(_ context.Context, message string) (string, error) {
	signature, err := crypto.Sign(accounts.TextHash([]byte(message)), s.key)
	if err != nil {
		return "", err
	}
	signature[64] += 27
	return "0x" + common.Bytes2Hex(signature), nil
}

func TestNativeSellerExtensionsAndPaywall(t *testing.T) {
	var paymentCalls, handled atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/x402/config":
			json.NewEncoder(w).Encode(sampleConfig())
		case "/v1/x402/supported":
			fmt.Fprint(w, `{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:8453"}]}`)
		default:
			paymentCalls.Add(1)
			t.Error("unexpected payment processing", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer api.Close()
	options := inflow.Options{BaseURL: api.URL, APIKey: "key"}
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	facilitator, err := NewFacilitator(options)
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := client.SchemeRegistrations(context.Background(), RegistrationOptions{Schemes: []string{"exact"}})
	if err != nil {
		t.Fatal(err)
	}
	serverOptions := []foundation.ResourceServerOption{foundation.WithFacilitatorClient(facilitator)}
	for _, registration := range registrations {
		serverOptions = append(serverOptions, foundation.WithSchemeServer(registration.Network, registration.Server))
	}
	storage := signinwithx.NewInMemoryStorage()
	extension, err := signinwithx.CreateResourceServerExtension(signinwithx.ServerOptions{Storage: storage, Origin: "https://seller.example"})
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := bazaar.DeclareDiscoveryExtension(bazaar.MethodGET, map[string]interface{}{"query": "widgets"}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := ProtectRequest(RequestPricingServer{
		RoutePattern: "GET /widgets/:id", Options: serverOptions,
		Configure: func(server *xhttp.HTTPServer) {
			server.RegisterExtension(extension)
			server.RegisterExtension(bazaar.BazaarResourceServerExtension)
			server.RegisterPaywallProvider(xhttp.NewPaywallBuilder().WithNetwork(&xhttp.EVMPaywallHandler{}).WithConfig(&xhttp.PaywallConfig{AppName: "Widget Shop"}).Build())
		},
	}, inflow.RequestPricingOptions{}, func(r *http.Request, _ []byte) (xhttp.RouteConfig, error) {
		route, err := client.Route(r.Context(), RouteOptions{AcceptsOptions: AcceptsOptions{Price: PriceSpec{Amount: "0.50 USDC"}, Schemes: []string{"exact"}}})
		if err != nil {
			return route, err
		}
		route.Extensions = signinwithx.DeclareExtension(signinwithx.DeclareOptions{Networks: []string{"eip155:8453"}})
		route.Extensions["bazaar"] = discovery
		for key, value := range buildercode.DeclareBuilderCodeExtension("widget_shop") {
			route.Extensions[key] = value
		}
		return route, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { handled.Add(1); fmt.Fprint(w, "widgets") }))
	if err != nil {
		t.Fatal(err)
	}
	call := func(header string, browser bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://seller.example/widgets/123", nil)
		if header != "" {
			r.Header.Set("SIGN-IN-WITH-X", header)
		}
		if browser {
			r.Header.Set("Accept", "text/html")
			r.Header.Set("User-Agent", "Mozilla/5.0")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	unpaid := call("", false)
	required := pricingRequirements(t, unpaid)
	if !strings.Contains(string(encode(t, required.Extensions["bazaar"])), `"routeTemplate":"/widgets/:id"`) {
		t.Fatal("lost Bazaar route template")
	}
	if !strings.Contains(string(encode(t, required.Extensions["builder-code"])), `"a":"widget_shop"`) {
		t.Fatal("lost explicit builder attribution")
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := extensionSigner{key: key}
	if err := storage.RecordPayment(context.Background(), "/widgets/123", signer.Address()); err != nil {
		t.Fatal(err)
	}
	header, err := signinwithx.CreateHeader(context.Background(), required.Extensions[signinwithx.ExtensionKey], signer, "https://seller.example/widgets/123")
	if err != nil {
		t.Fatal(err)
	}
	paid := call(header, false)
	if paid.Code != 200 || paid.Body.String() != "widgets" || paymentCalls.Load() != 0 {
		t.Fatalf("native authentication failed: %d %s", paid.Code, paid.Body)
	}
	replay := call(header, false)
	if replay.Code != 402 || handled.Load() != 1 {
		t.Fatal("replayed signature granted access")
	}
	invalid := call("invalid", false)
	if invalid.Code != 402 || handled.Load() != 1 {
		t.Fatal("invalid signature granted access")
	}
	browser := call("", true)
	if browser.Code != 402 || !strings.Contains(browser.Header().Get("Content-Type"), "text/html") || !strings.Contains(browser.Body.String(), "Widget Shop") {
		t.Fatalf("native paywall missing: %d %s", browser.Code, browser.Body)
	}
	if paymentCalls.Load() != 0 || handled.Load() != 1 {
		t.Fatal("unpaid request performed payment or fulfillment")
	}
}
