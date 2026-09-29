package main

import (
	"context"
	"errors"
	"net/http"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
)

func main() {
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
	_, err = client.Fulfil(ctx, challenge, buyer.PaymentOptions{})
	var paymentError *buyer.Error
	if !errors.As(err, &paymentError) || paymentError.Code != buyer.Cancelled || !errors.Is(err, context.Canceled) {
		panic("buyer cancellation contract failed")
	}
}
