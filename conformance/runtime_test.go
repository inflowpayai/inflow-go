package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	mb "github.com/inflowpayai/inflow-go/mpp/buyer"
	ms "github.com/inflowpayai/inflow-go/mpp/seller"
	xb "github.com/inflowpayai/inflow-go/x402/buyer"
	xs "github.com/inflowpayai/inflow-go/x402/seller"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func publicCall(ctx context.Context, product string, options inflow.Options) error {
	switch product {
	case "mpp-buyer":
		client, err := mb.New(mb.Options{Options: options})
		if err != nil {
			return err
		}
		_, err = client.Prepare(ctx, mpp.Challenge{ID: "test", Realm: "seller.example", Method: "inflow", Intent: "charge", Request: "e30"}, mb.PaymentOptions{})
		return err
	case "mpp-seller":
		client, err := ms.New(options)
		if err != nil {
			return err
		}
		return client.Load(ctx)
	case "x402-buyer":
		client, err := xb.New(xb.Options{Options: options})
		if err != nil {
			return err
		}
		_, err = client.Supported(ctx)
		return err
	case "x402-seller":
		client, err := xs.New(options)
		if err != nil {
			return err
		}
		_, err = client.Config(ctx)
		return err
	default:
		return errors.New("unknown runtime client")
	}
}

func executeRuntime(ctx context.Context, operation string, raw json.RawMessage) (any, error) {
	var input struct {
		Product     string
		Environment inflow.Environment
		BaseURL     string `json:"base_url"`
		Tokens      []string
	}
	if err := decode(raw, &input); err != nil {
		return nil, err
	}
	if operation == "runtime.environment" {
		var destinations []string
		options := inflow.Options{Environment: input.Environment, BaseURL: input.BaseURL, APIKey: "test-only-key", Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			destinations = append(destinations, r.Method+" "+r.URL.String())
			if r.Body != nil {
				r.Body.Close()
			}
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		err := publicCall(ctx, input.Product, options)
		var api *inflow.APIError
		if !errors.As(err, &api) || api.HTTPStatus != 401 {
			return nil, errors.New("environment probe did not produce the expected HTTP rejection")
		}
		return map[string]any{"destinations": destinations}, nil
	}
	if operation != "runtime.request" {
		return nil, errors.New("unknown runtime operation")
	}
	options, err := platformOptions(raw)
	if err != nil {
		return nil, err
	}
	calls := 0
	if input.Tokens != nil {
		options.AccessToken = func(context.Context) (string, error) {
			if calls >= len(input.Tokens) {
				return "", errors.New("unexpected credential retrieval")
			}
			token := input.Tokens[calls]
			calls++
			return token, nil
		}
	}
	err = publicCall(ctx, input.Product, options)
	var api *inflow.APIError
	if !errors.As(err, &api) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("runtime case must observe an HTTP rejection")
	}
	sensitive := []string{}
	for _, name := range []string{"authorization", "cookie", "set-cookie", "x-api-key"} {
		if api.Headers.Get(name) != "" {
			sensitive = append(sensitive, name)
		}
	}
	return map[string]any{"code": api.Code, "message": api.Message, "http_status": api.HTTPStatus, "endpoint": api.Endpoint, "token_calls": calls, "request_id": api.RequestID, "sensitive_headers": sensitive}, nil
}
