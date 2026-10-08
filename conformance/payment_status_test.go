package conformance

import (
	"context"
	"encoding/json"

	inflow "github.com/inflowpayai/inflow-go"
	mb "github.com/inflowpayai/inflow-go/mpp/buyer"
	xb "github.com/inflowpayai/inflow-go/x402/buyer"
)

func executePaymentStatus(ctx context.Context, operation string, raw json.RawMessage) (result any, outcome error) {
	var input struct {
		TransactionID string `json:"transaction_id"`
		Reads         *int   `json:"reads"`
		Retries       int    `json:"retries"`
	}
	if err := decode(raw, &input); err != nil {
		return nil, err
	}
	defer watchInput(&input, &outcome)()
	options, err := platformOptions(raw)
	if err != nil {
		return nil, err
	}
	var read func(context.Context, string, inflow.PaymentStatusOptions) (inflow.PaymentStatus, error)
	if operation == "mpp.buyer.payment-status" {
		client, err := mb.New(mb.Options{Options: options})
		if err != nil {
			return nil, err
		}
		read = client.PaymentStatus
	} else {
		client, err := xb.New(xb.Options{Options: options})
		if err != nil {
			return nil, err
		}
		// Node loads capabilities during construction; Go constructors do not perform requests.
		if _, err = client.Supported(ctx); err != nil {
			return nil, err
		}
		read = client.PaymentStatus
	}
	count := 1
	if input.Reads != nil {
		count = *input.Reads
	}
	values := []inflow.PaymentStatus{}
	for range count {
		value, err := read(ctx, input.TransactionID, inflow.PaymentStatusOptions{Retries: input.Retries})
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}
