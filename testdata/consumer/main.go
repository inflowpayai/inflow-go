package main

import (
	"context"
	"errors"
	"net/http"

	inflow "github.com/inflowpayai/inflow-go"
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
}
