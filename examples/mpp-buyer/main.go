package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/buyer"
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
		target = "http://127.0.0.1:3000/api/widgets"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Requesting resource; approve in InFlow if requested.")
	// Do handles the 402 challenge, waits for approval when needed, then sends one
	// paid request. SubscriptionID authorizes an existing subscription instead of buying one.
	response, err := client.Do(request, buyer.PaymentOptions{SubscriptionID: getenv("SUBSCRIPTION_ID")})
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
	// Decode the seller's reported result; decoding does not verify settlement independently.
	if encoded := response.Header.Get("Payment-Receipt"); encoded != "" {
		receipt, err := mpp.DecodeReceipt(encoded)
		if err != nil {
			return fmt.Errorf("cannot read the seller's payment receipt: %w", err)
		}
		fmt.Fprintf(out, "Seller receipt: status=%s method=%s reference=%s\n", receipt.Status, receipt.Method, receipt.Reference)
	} else {
		fmt.Fprintln(out, "No payment receipt supplied; a free response normally has none.")
	}
	return nil
}
