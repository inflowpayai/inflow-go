package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402/seller"
	foundation "github.com/x402-foundation/x402/go/v2"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
)

func main() {
	if err := run(os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(getenv func(string) string) error {
	key := getenv("INFLOW_API_KEY")
	if key == "" {
		return errors.New("set INFLOW_API_KEY to your Sandbox Seller account API key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handler, err := newHandler(ctx, inflow.Options{Environment: inflow.Sandbox, APIKey: key, BaseURL: getenv("INFLOW_BASE_URL")}, foundation.PaymentFlowName(getenv("X402_PAYMENT_FLOW")))
	if err != nil {
		return err
	}
	address := getenv("LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:3001"
	}
	log.Printf("x402 seller listening on http://%s", address)
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

func newHandler(ctx context.Context, options inflow.Options, flow foundation.PaymentFlowName) (http.Handler, error) {
	// The client builds offers from Seller configuration. The facilitator delegates
	// payment verification and settlement to InFlow; it is not a server you must host.
	client, clientError := seller.New(options)
	facilitator, facilitatorError := seller.NewFacilitator(options)
	if err := errors.Join(clientError, facilitatorError); err != nil {
		return nil, err
	}
	// Register implementations for the schemes/networks the middleware can advertise.
	registrations, err := client.SchemeRegistrations(ctx, seller.RegistrationOptions{Schemes: []string{"balance", "exact"}})
	if err != nil {
		return nil, err
	}
	// Resolve the human price into atomic amounts and recipients from Seller configuration.
	route, err := client.Route(ctx, seller.RouteOptions{
		AcceptsOptions: seller.AcceptsOptions{Price: seller.PriceSpec{Amount: "0.01 USDC"}, Schemes: []string{"balance", "exact"}, PaymentFlow: flow},
	})
	if err != nil {
		return nil, err
	}
	if len(route.Accepts) == 0 {
		return nil, errors.New("Seller configuration has no compatible payment offers")
	}
	serverOptions := []foundation.ResourceServerOption{foundation.WithFacilitatorClient(facilitator)}
	for _, registration := range registrations {
		serverOptions = append(serverOptions, foundation.WithSchemeServer(registration.Network, registration.Server))
	}
	resource := xhttp.Newx402HTTPResourceServer(xhttp.RoutesConfig{"GET /api/widgets": route}, serverOptions...)
	// Fail startup on incompatible configuration rather than fail the first customer's request.
	if err := resource.Initialize(ctx); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	// Constant valid options and a non-nil handler; route errors are returned per request.
	protected, _ := seller.ProtectRequest(seller.RequestPricingServer{Options: serverOptions}, inflow.RequestPricingOptions{}, func(r *http.Request, _ []byte) (xhttp.RouteConfig, error) {
		price := "0.01 USDC"
		if r.URL.Query().Get("bundle") == "large" {
			price = "0.02 USDC"
		}
		return client.Route(r.Context(), seller.RouteOptions{
			AcceptsOptions: seller.AcceptsOptions{Price: seller.PriceSpec{Amount: price}, Schemes: []string{"balance", "exact"}, PaymentFlow: flow},
		})
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("bundle") == "large" {
			fmt.Fprintln(w, `{"widgets":[1,2,3,4,5,6]}`)
		} else {
			fmt.Fprintln(w, `{"widgets":[1,2,3]}`)
		}
	}))
	mux.Handle("GET /api/widgets", protected)
	mux.HandleFunc("GET /free", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "No payment required") })
	// Authorization settles after a successful handler; upfront settles before it.
	// Neither flow undoes application side effects or automatically refunds failures.
	return mux, nil
}
