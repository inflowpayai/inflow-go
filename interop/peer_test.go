package interop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	mb "github.com/inflowpayai/inflow-go/mpp/buyer"
	ms "github.com/inflowpayai/inflow-go/mpp/seller"
	"github.com/inflowpayai/inflow-go/x402"
	xb "github.com/inflowpayai/inflow-go/x402/buyer"
	xs "github.com/inflowpayai/inflow-go/x402/seller"
	foundation "github.com/x402-foundation/x402/go/v2"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

type settings struct {
	Role, Protocol, Platform, Target, Variant string
	HandlerStatus                             int
	SubscriptionID                            string
}

func TestMain(m *testing.M) {
	if len(os.Args) != 2 || os.Args[1] != "--peer" {
		os.Exit(m.Run())
	}
	if err := runPeer(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loopback(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.User == nil
}

func runPeer() error {
	var s settings
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 8192)).Decode(&s); err != nil {
		return err
	}
	if !loopback(s.Platform) || (s.Role == "buyer" && !loopback(s.Target)) {
		return fmt.Errorf("loopback endpoints required")
	}
	if s.Protocol != "mpp" && s.Protocol != "x402" {
		return fmt.Errorf("unknown protocol")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	options := inflow.Options{BaseURL: s.Platform, APIKey: "test-only-" + s.Role + "-key", Timeout: 5 * time.Second}
	if s.Role == "buyer" {
		request, err := http.NewRequestWithContext(ctx, "GET", s.Target, nil)
		if err != nil {
			return err
		}
		request.Header.Set("X-App-Session", "test-only-session")
		var response *http.Response
		if s.Protocol == "mpp" {
			client, err := mb.New(mb.Options{Options: options, PollInterval: time.Nanosecond, WaitTimeout: 5 * time.Second})
			if err != nil {
				return err
			}
			response, err = client.Do(request, mb.PaymentOptions{SubscriptionID: s.SubscriptionID})
			if err != nil {
				return err
			}
		} else {
			client, err := xb.New(xb.Options{Options: options, PollInterval: time.Nanosecond, WaitTimeout: 5 * time.Second})
			if err != nil {
				return err
			}
			response, err = client.Do(request, xb.SignOptions{})
			if err != nil {
				return err
			}
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
		if err != nil {
			return err
		}
		var receipt any
		if raw := response.Header.Get("Payment-Receipt"); raw != "" {
			receipt, err = mpp.DecodeReceipt(raw)
		}
		if raw := response.Header.Get("PAYMENT-RESPONSE"); raw != "" {
			var data []byte
			data, err = base64.StdEncoding.DecodeString(raw)
			if err == nil {
				var value x402.SettleResponse
				err = json.Unmarshal(data, &value)
				receipt = value
			}
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": response.StatusCode, "body": string(body), "receipt": receipt, "cache": response.Header.Get("Cache-Control")})
	}
	if s.Role != "seller" {
		return fmt.Errorf("unknown role")
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event, err := http.NewRequestWithContext(r.Context(), "POST", s.Platform+"/handler", nil)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		response, err := http.DefaultClient.Do(event)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		response.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.HandlerStatus)
		fmt.Fprint(w, `{"paidResource":true}`)
	})
	var handler http.Handler
	if s.Protocol == "mpp" {
		client, err := ms.New(options)
		if err != nil {
			return err
		}
		offer := ms.Offer{Charge: &mpp.ChargeRequest{Amount: "0.01", Currency: "USDC"}}
		if s.Variant == "tempo" {
			offer = ms.Offer{Tempo: &mpp.TempoRequest{Amount: "10000", Currency: "0x20c0000000000000000000000000000000000000", Recipient: "0x1111111111111111111111111111111111111111"}}
		}
		if s.Variant == "subscription" {
			offer = ms.Offer{Subscription: &mpp.SubscriptionRequest{ChargeRequest: mpp.ChargeRequest{Amount: "0.01", Currency: "USDC"}, PeriodUnit: "month", PeriodCount: 1, SubscriptionExpires: "2099-01-01T00:00:00Z"}}
		}
		handler, err = client.Protect(ms.Route{Realm: "interop", SecretKey: "test-only-binding-secret-at-least-32-bytes", Offers: []ms.Offer{offer}}, next)
		if err != nil {
			return err
		}
	} else {
		client, err := xs.New(options)
		if err != nil {
			return err
		}
		facilitator, err := xs.NewFacilitator(options)
		if err != nil {
			return err
		}
		schemes := []string{s.Variant}
		registrations, err := client.SchemeRegistrations(ctx, xs.RegistrationOptions{Schemes: schemes})
		if err != nil {
			return err
		}
		route, err := client.Route(ctx, xs.RouteOptions{AcceptsOptions: xs.AcceptsOptions{Price: xs.PriceSpec{Amount: "0.01 USDC"}, Schemes: schemes}})
		if err != nil {
			return err
		}
		resource := xhttp.Newx402HTTPResourceServer(xhttp.RoutesConfig{"GET /paid": route}, foundation.WithFacilitatorClient(facilitator))
		for _, registration := range registrations {
			resource.Register(registration.Network, registration.Server)
		}
		if err = resource.Initialize(ctx); err != nil {
			return err
		}
		handler = nethttp.PaymentMiddlewareFromHTTPServer(resource, nethttp.WithSyncFacilitatorOnStart(false))(next)
	}
	protected := handler
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("X-App-Session") != "test-only-session" {
			http.Error(w, "authentication boundary failure", 500)
			return
		}
		protected.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String() + "/paid"}); err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(listener)
}

func TestLoopback(t *testing.T) {
	for _, value := range []string{"https://example.com", "http://localhost", "http://user@127.0.0.1", ":bad"} {
		if loopback(value) {
			t.Fatal(value)
		}
	}
	if !loopback("http://127.0.0.1:123/paid") {
		t.Fatal("loopback rejected")
	}
}
