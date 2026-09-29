package main

import (
	"context"
	"errors"
	"net/http"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
	"github.com/inflowpayai/inflow-go/mpp/seller"
	"github.com/inflowpayai/inflow-go/x402"
	x402buyer "github.com/inflowpayai/inflow-go/x402/buyer"
	foundation "github.com/x402-foundation/x402/go/v2"
)

func main() {
	var requirement foundation.PaymentRequirements = x402.PaymentRequirements{Scheme: x402.SchemeBalance, Network: x402.NetworkInflow}
	id, idErr := x402.GeneratePaymentID(x402.DefaultPaymentIDPrefix)
	if idErr != nil || x402.PaymentIdentifierEntry(x402.DeclarePaymentIdentifier(), id) == nil || requirement.Scheme != "balance" {
		panic("x402 core contract failed")
	}
	options := inflow.Options{
		Environment: inflow.Sandbox,
		AccessToken: func(context.Context) (string, error) { return "test-only-token", nil },
		Transport:   http.DefaultTransport,
	}
	if options.Environment != inflow.Sandbox {
		panic("unexpected environment")
	}
	var err error = &inflow.APIError{Message: "cancelled", Cause: context.Canceled}
	if !errors.Is(err, context.Canceled) {
		panic("missing cancellation cause")
	}
	request := mpp.ChargeRequest{Amount: "10.5", Currency: "USDC"}
	if err := request.Validate(); err != nil {
		panic(err)
	}
	encoded, err := mpp.Encode(request)
	if err != nil {
		panic(err)
	}
	challenge := mpp.Challenge{ID: "consumer-test", Realm: "seller.example", Method: mpp.MethodInflow, Intent: mpp.IntentCharge, Request: encoded}
	header, err := mpp.RenderChallenge(challenge)
	if err != nil {
		panic(err)
	}
	parsed, err := mpp.ParseChallenges([]string{header})
	if err != nil || len(parsed) != 1 || parsed[0].Request != encoded {
		panic("challenge round trip failed")
	}
	client, err := buyer.New(buyer.Options{Options: options})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x402Client, err := x402buyer.New(x402buyer.Options{Options: options, External: foundation.Newx402Client()})
	if err != nil {
		panic(err)
	}
	if _, err := x402Client.Supported(ctx); !errors.Is(err, context.Canceled) {
		panic("x402 buyer cancellation contract failed")
	}
	_, err = client.Fulfil(ctx, challenge, buyer.PaymentOptions{})
	var paymentError *buyer.Error
	if !errors.As(err, &paymentError) || paymentError.Code != buyer.Cancelled || !errors.Is(err, context.Canceled) {
		panic("buyer cancellation contract failed")
	}
	sellerClient, err := seller.New(options)
	if err != nil {
		panic(err)
	}
	if err := sellerClient.Load(ctx); !errors.Is(err, context.Canceled) {
		panic("seller cancellation contract failed")
	}
}
