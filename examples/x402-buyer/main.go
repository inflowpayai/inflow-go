package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/inflowpayai/inflow-go/x402/buyer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	key := getenv("INFLOW_API_KEY")
	if key == "" {
		return errors.New("set INFLOW_API_KEY to your Sandbox account API key")
	}
	// These credentials authenticate to InFlow, not to the seller's resource.
	client, err := buyer.New(buyer.Options{Options: inflow.Options{
		Environment: inflow.Sandbox, APIKey: key, BaseURL: getenv("INFLOW_BASE_URL"),
	}})
	if err != nil {
		return err
	}
	target := getenv("TARGET_URL")
	if target == "" {
		target = "http://127.0.0.1:3001/api/widgets"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Requesting resource; approve in InFlow if requested.")
	// Do selects a managed offer, waits for approval when needed, and sends the
	// signed payment in one paid request. A second 402 does not trigger another payment.
	response, err := client.Do(request, buyer.SignOptions{})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	fmt.Fprintf(out, "HTTP %d\n", response.StatusCode)
	if _, err := io.Copy(out, response.Body); err != nil {
		return err
	}
	fmt.Fprintln(out)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("resource returned HTTP %d; inspect the outcome before retrying payment", response.StatusCode)
	}
	// This is the seller's reported settlement result, not independent verification.
	if encoded := response.Header.Get(x402.HeaderPaymentResponse); encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("cannot decode the seller's settlement header: %w", err)
		}
		var receipt x402.SettleResponse
		if err := json.Unmarshal(data, &receipt); err != nil {
			return fmt.Errorf("cannot read the seller's settlement response: %w", err)
		}
		if !receipt.Success {
			return fmt.Errorf("seller reported unsuccessful settlement: %s; inspect the outcome before retrying", receipt.ErrorReason)
		}
		fmt.Fprintf(out, "Seller settlement: success=%t network=%s transaction=%s\n", receipt.Success, receipt.Network, receipt.Transaction)
	} else {
		fmt.Fprintln(out, "No settlement receipt supplied; a free response normally has none.")
	}
	return nil
}
