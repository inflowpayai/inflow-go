package seller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
)

func TestSellerAPIKeyProvider(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "provider-key" {
			t.Error("missing provider key")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	options := inflow.Options{BaseURL: server.URL, APIKeyProvider: func(context.Context) (string, error) { calls++; return "provider-key", nil }}
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Config(context.Background()); err != nil {
		t.Fatal(err)
	}
	facilitator, err := NewFacilitator(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = facilitator.GetSupported(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("provider calls: %d", calls)
	}
}
