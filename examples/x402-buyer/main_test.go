package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
)

func TestRun(t *testing.T) {
	for _, scenario := range []string{"free", "receipt", "bad-encoding", "bad-json", "failed-settlement", "rejected", "truncated", "missing-key", "bad-key", "bad-target", "cancelled", "default-target"} {
		t.Run(scenario, func(t *testing.T) {
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "" {
					t.Error("platform key leaked")
				}
				if scenario == "rejected" {
					w.WriteHeader(402)
					return
				}
				if scenario == "truncated" {
					w.Header().Set("Content-Length", "100")
				}
				if scenario == "receipt" {
					w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString([]byte(`{"success":true,"network":"inflow:1","transaction":"example-receipt"}`)))
				}
				switch scenario {
				case "bad-encoding":
					w.Header().Set("PAYMENT-RESPONSE", "not-base64!")
				case "bad-json":
					w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString([]byte("not-json")))
				case "failed-settlement":
					w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString([]byte(`{"success":false,"errorReason":"payment_rejected"}`)))
				}
				fmt.Fprint(w, "resource-content")
			}))
			defer resource.Close()
			env := map[string]string{"INFLOW_API_KEY": "test-only-key", "TARGET_URL": resource.URL}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "missing-key":
				delete(env, "INFLOW_API_KEY")
			case "bad-key":
				env["INFLOW_API_KEY"] = "bad key"
			case "bad-target":
				env["TARGET_URL"] = "://"
			case "cancelled":
				cancel()
			case "default-target":
				delete(env, "TARGET_URL")
				cancel()
			}
			var out bytes.Buffer
			err := run(ctx, func(k string) string { return env[k] }, &out)
			success := scenario == "free" || scenario == "receipt"
			if (err == nil) != success {
				t.Fatalf("error=%v output=%s", err, &out)
			}
			if success && !strings.Contains(out.String(), "resource-content") {
				t.Fatal(out.String())
			}
			if scenario == "receipt" && !strings.Contains(out.String(), "example-receipt") {
				t.Fatal(out.String())
			}
			if scenario == "free" && !strings.Contains(out.String(), "No settlement receipt supplied") {
				t.Fatal(out.String())
			}
			if scenario == "failed-settlement" && !strings.Contains(err.Error(), "payment_rejected") {
				t.Fatal(err)
			}
		})
	}
	// A non-402 error must be reported without attempting a payment.
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer resource.Close()
	env := map[string]string{"INFLOW_API_KEY": "test-only-key", "TARGET_URL": resource.URL}
	if err := run(context.Background(), func(k string) string { return env[k] }, io.Discard); err == nil {
		t.Fatal("accepted forbidden response")
	}
}

func TestPaidRequest(t *testing.T) {
	data, err := os.ReadFile("../../x402/buyer/testdata/buyer.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input struct {
			Challenge   mpp.Challenge
			Requirement json.RawMessage
		}
		Platform struct {
			Exchanges []struct {
				Request  struct{ Method, Path string }
				Response struct {
					Status int
					JSON   json.RawMessage
				}
			}
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	fixture := cases[0]
	var calls atomic.Int32
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(fixture.Platform.Exchanges) {
			t.Error("extra platform request")
			w.WriteHeader(400)
			return
		}
		exchange := fixture.Platform.Exchanges[index]
		if r.URL.Path != exchange.Request.Path || r.Method != exchange.Request.Method || r.Header.Get("X-API-Key") != "test-only-key" {
			t.Error("unexpected platform request", r.Method, r.URL)
		}
		w.WriteHeader(exchange.Response.Status)
		w.Write(exchange.Response.JSON)
	}))
	defer platform.Close()
	var resources atomic.Int32
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Error("platform key leaked")
		}
		if resources.Add(1) == 1 {
			required := []byte(fmt.Sprintf("{\"x402Version\":2,\"resource\":{\"url\":\"https://seller.example/data\",\"mimeType\":\"application/json\"},\"accepts\":[%s]}", fixture.Input.Requirement))
			w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(required))
			w.WriteHeader(402)
			return
		}
		if r.Header.Get("PAYMENT-SIGNATURE") == "" {
			t.Error("missing payment")
		}
		fmt.Fprint(w, "paid-content")
	}))
	defer resource.Close()
	env := map[string]string{"INFLOW_API_KEY": "test-only-key", "INFLOW_BASE_URL": platform.URL, "TARGET_URL": resource.URL}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	if resources.Load() != 2 || calls.Load() != int32(len(fixture.Platform.Exchanges)) || !strings.Contains(out.String(), "paid-content") {
		t.Fatal(calls.Load(), resources.Load(), out.String())
	}
}
