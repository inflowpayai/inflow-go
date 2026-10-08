package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

func main() {
	if err := run(os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(getenv func(string) string) error {
	key, secret := getenv("INFLOW_API_KEY"), getenv("MPP_SECRET_KEY")
	if key == "" || secret == "" {
		return errors.New("set INFLOW_API_KEY to your Sandbox Seller key and MPP_SECRET_KEY to a separate private signing key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handler, err := newHandler(ctx, inflow.Options{Environment: inflow.Sandbox, APIKey: key, BaseURL: getenv("INFLOW_BASE_URL")}, secret, getenv("MPP_PAYMENT_METHOD"))
	if err != nil {
		return err
	}
	address := getenv("LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:3000"
	}
	log.Printf("MPP seller listening on http://%s", address)
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

func newHandler(ctx context.Context, options inflow.Options, secret, method string) (http.Handler, error) {
	client, err := seller.New(options)
	if err != nil {
		return nil, err
	}
	// Check the Seller key and cache platform configuration before accepting requests.
	if err := client.Load(ctx); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	offers := map[string]seller.Offer{
		"GET /api/widgets": {Charge: &mpp.ChargeRequest{Amount: "0.01", Currency: "USDC"}},
		"GET /api/subscribe": {Subscription: &mpp.SubscriptionRequest{
			// Recurring terms, not a one-time charge. Persist stable terms in a real service.
			ChargeRequest: mpp.ChargeRequest{Amount: "1.00", Currency: "USDC"},
			PeriodUnit:    "month", PeriodCount: 1,
			SubscriptionExpires: time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339),
		}},
	}
	if method == "stripe" {
		// External Buyers supply Stripe Shared Payment Tokens. InFlow processes them;
		// this application needs neither a Stripe secret key nor a token-creation endpoint.
		offers = map[string]seller.Offer{"GET /api/widgets": {Stripe: &seller.StripeOffer{Amount: "1.25"}}}
	} else if method == "card" {
		// InFlow supplies merchant settings and the public encryption key. The Seller
		// forwards encrypted credentials; it does not receive or decrypt card details.
		offers = map[string]seller.Offer{"GET /api/widgets": {Card: &seller.CardOffer{Amount: "1.25"}}}
	} else if method != "" && method != "inflow" {
		return nil, errors.New("MPP_PAYMENT_METHOD must be inflow, stripe, or card")
	}
	for path, offer := range offers {
		// Distinct opaque values prevent a credential for one route being reused on another.
		opaque := base64.RawURLEncoding.EncodeToString([]byte(path))
		if _, err := client.Prepare(ctx, offer); err != nil {
			return nil, err
		}
		// Protect issues unpaid challenges and processes payment before this handler runs.
		// A handler error cannot undo that payment.
		protected, err := client.Protect(seller.Route{
			Realm: "inflow-go-example", SecretKey: secret, Offers: []seller.Offer{offer}, Opaque: &opaque,
		}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"ok":true,"message":"Paid resource accessed"}`)
		}))
		if err != nil {
			return nil, err
		}
		mux.Handle(path, protected)
	}
	// Routes mounted without Protect remain free.
	mux.HandleFunc("GET /free", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "No payment required") })
	return mux, nil
}
