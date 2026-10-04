package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	mb "github.com/inflowpayai/inflow-go/mpp/buyer"
	ms "github.com/inflowpayai/inflow-go/mpp/seller"
	xb "github.com/inflowpayai/inflow-go/x402/buyer"
	xs "github.com/inflowpayai/inflow-go/x402/seller"
)

// The runner executes this test binary with --adapter. No adapter is shipped as
// SDK code, and neither expected results nor HTTP response scripts reach it.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--adapter" {
		if err := serve(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type request struct {
	Version   string          `json:"adapter_version"`
	Sequence  int             `json:"sequence"`
	CaseID    string          `json:"case_id"`
	Operation string          `json:"operation"`
	Input     json.RawMessage `json:"input"`
}

type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"http_status,omitempty"`
	Details any    `json:"details,omitempty"`
}

func serve(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var r request
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return err
		}
		response := map[string]any{"adapter_version": "1", "sequence": r.Sequence, "case_id": r.CaseID}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		result, err := execute(ctx, r)
		cancel()
		if err != nil {
			response["error"] = classify(err, r)
		} else {
			response["result"] = result
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func decode(raw json.RawMessage, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(target)
}

func watchInput(input any, outcome *error) func() {
	before, err := json.Marshal(input)
	return func() {
		after, next := json.Marshal(input)
		if err != nil || next != nil || !bytes.Equal(before, after) {
			*outcome = errors.New("adapter input mutated or could not be compared")
		}
	}
}

func execute(ctx context.Context, r request) (any, error) {
	if r.Version != "1" {
		return nil, errors.New("unsupported adapter version")
	}
	switch {
	case strings.HasPrefix(r.Operation, "tap."):
		return executeTAP(ctx, r.Operation, r.Input)
	case strings.HasPrefix(r.Operation, "mpp."):
		return executeMPP(ctx, r.Operation, r.Input)
	case strings.HasPrefix(r.Operation, "x402."):
		return executeX402(ctx, r.Operation, r.Input)
	case strings.HasPrefix(r.Operation, "runtime."):
		return executeRuntime(ctx, r.Operation, r.Input)
	default:
		return nil, errors.New("unsupported operation")
	}
}

func platformOptions(raw json.RawMessage) (inflow.Options, error) {
	var input struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decode(raw, &input); err != nil {
		return inflow.Options{}, err
	}
	u, err := url.Parse(input.BaseURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return inflow.Options{}, errors.New("adapter requires a loopback platform URL")
	}
	return inflow.Options{BaseURL: input.BaseURL, APIKey: input.APIKey}, nil
}

func classify(err error, r request) failure {
	f := failure{Code: "ADAPTER_ERROR", Message: err.Error()}
	var api *inflow.APIError
	var codec *mpp.CodecError
	var mp *mb.Error
	var sp *ms.Error
	var xp *xb.Error
	var price *xs.PriceError
	switch {
	case errors.As(err, &api):
		if strings.HasPrefix(r.Operation, "x402.") {
			return failure{Code: "api-error", Message: "InFlow API request failed.", Status: api.HTTPStatus, Details: map[string]any{"body": api.Body}}
		}
		return failure{Code: api.Code, Message: api.Message, Status: api.HTTPStatus}
	case errors.As(err, &mp):
		f.Code = string(mp.Code)
		if mp.Code == mb.Timeout || mp.Code == mb.Expired {
			if mp.TransactionID != "" {
				f.Details = map[string]any{"transaction_id": mp.TransactionID}
			}
		}
		if mp.Code == mb.Failed && len(mp.Problem) > 0 {
			f.Details = map[string]any{"problem": mp.Problem}
		}
	case errors.As(err, &codec):
		f.Code = "invalid-input"
		if r.Operation == "mpp.core.decode-credential" {
			f.Code = "invalid-credential"
		}
	case errors.As(err, &sp):
		f.Code = sp.Code
		if f.Code == "ambiguous-rail" || f.Code == "instrument-required" {
			f.Code = "unsupported-capability"
		}
		var input struct {
			IncludeProblem *bool `json:"include_problem"`
		}
		if e := decode(r.Input, &input); e != nil {
			return failure{Code: "ADAPTER_ERROR", Message: e.Error()}
		}
		if len(sp.Problem) > 0 && (input.IncludeProblem == nil || *input.IncludeProblem) {
			f.Details = map[string]any{"problem": sp.Problem}
		}
	case errors.As(err, &xp):
		f.Code = xp.Code
		if xp.Code == "payment-failed" {
			f.Details = map[string]any{"status": xp.Status}
		}
	case errors.As(err, &price):
		f.Code = "invalid-input"
	default:
		return f
	}
	message, ok := map[string]string{"invalid-input": "Invalid input.", "invalid-credential": "Invalid credential.", "payment-failed": "Payment failed.", "payment-expired": "Payment expired.", "payment-timeout": "Payment timed out.", "payment-cancelled": "Payment cancelled.", "unsupported-capability": "Unsupported payment capability."}[f.Code]
	if !ok {
		return failure{Code: "ADAPTER_ERROR", Message: err.Error()}
	}
	f.Message = message
	return f
}
