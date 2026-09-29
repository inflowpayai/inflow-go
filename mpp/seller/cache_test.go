package seller_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
	"github.com/inflowpayai/inflow-go/mpp/seller"
)

func paidHandler(t *testing.T, next http.Handler) (http.Handler, string) {
	t.Helper()
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/mpp/config":
			fmt.Fprint(w, configJSON)
		case "/v1/mpp/validate":
			writeValidation(w, r)
		case "/v1/mpp/broadcast":
			fmt.Fprint(w, receiptJSON())
		default:
			t.Error("unexpected platform request", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(platform.Close)
	h, err := clientFor(t, platform).Protect(seller.Route{Realm: "seller.example", SecretKey: "test-only-key", Offers: []seller.Offer{chargeOffer()}}, next)
	if err != nil {
		t.Fatal(err)
	}
	challengeResponse := httptest.NewRecorder()
	h.ServeHTTP(challengeResponse, httptest.NewRequest("GET", "/paid", nil))
	if challengeResponse.Code != 402 || challengeResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("challenge cache policy changed")
	}
	challenges, err := mpp.ParseChallenges(challengeResponse.Header().Values("WWW-Authenticate"))
	if err != nil || len(challenges) != 1 {
		t.Fatal("missing challenge", err)
	}
	encoded, err := mpp.EncodeCredential(mpp.Credential{Challenge: challenges[0], Payload: map[string]any{"transactionId": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	return h, "Payment " + encoded
}

func TestPaidStreamAndInformationalHeaders(t *testing.T) {
	release := make(chan struct{})
	h, auth := paidHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Cache-Control", "max-age=60")
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Error(err)
		}
		fmt.Fprintln(w, "first")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintln(w, "second")
	}))
	resource := httptest.NewServer(h)
	defer resource.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hints := make(chan string, 1)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(code int, headers textproto.MIMEHeader) error {
		if code != http.StatusEarlyHints {
			t.Errorf("unexpected informational status %d", code)
		}
		hints <- headers.Get("Cache-Control")
		return nil
	}})
	request, _ := http.NewRequestWithContext(ctx, "GET", resource.URL, nil)
	request.Header.Set("Authorization", auth)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Cache-Control") != "max-age=60, private" {
		t.Fatal(response.Header)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatal("stream buffered or altered", line, err)
	}
	select {
	case value := <-hints:
		if value != "public, private" {
			t.Fatal(value)
		}
	default:
		t.Fatal("informational response missing")
	}
	release <- struct{}{}
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "second\n" {
		t.Fatal("stream completion changed", string(rest), err)
	}
}

func TestPaidCachePolicy(t *testing.T) {
	for _, mode := range []string{"write", "header", "empty", "flush", "copy"} {
		for _, policy := range []struct {
			name   string
			values []string
			want   string
		}{
			{"default", nil, "private"},
			{"public", []string{"public, max-age=300"}, "public, max-age=300, private"},
			{"no-store", []string{"no-store"}, "no-store, private"},
			{"private", []string{"max-age=60, PRIVATE"}, "max-age=60, PRIVATE"},
			{"private-first", []string{"PRIVATE, max-age=60"}, "PRIVATE, max-age=60"},
			{"multiple", []string{"max-age=60", "must-revalidate"}, "max-age=60, must-revalidate, private"},
			{"qualified-private", []string{`private="Payment-Receipt"`}, `private="Payment-Receipt", private`},
			{"quoted-field", []string{`private="foo, private, bar"`}, `private="foo, private, bar", private`},
			{"quoted-extension", []string{`custom="a\", private, b"`}, `custom="a\", private, b", private`},
		} {
			t.Run(mode+"/"+policy.name, func(t *testing.T) {
				h, auth := paidHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Del("Cache-Control")
					for _, value := range policy.values {
						w.Header().Add("Cache-Control", value)
					}
					switch mode {
					case "write":
						fmt.Fprint(w, "paid")
					case "header":
						w.WriteHeader(204)
					case "flush":
						if err := http.NewResponseController(w).Flush(); err != nil {
							t.Error(err)
						}
					case "copy":
						if _, err := io.Copy(w, struct{ io.Reader }{strings.NewReader("paid")}); err != nil {
							t.Error(err)
						}
					}
				}))
				resource := httptest.NewServer(h)
				defer resource.Close()
				request, _ := http.NewRequest("GET", resource.URL, nil)
				request.Header.Set("Authorization", auth)
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				status := 200
				if mode == "header" {
					status = 204
				}
				wantBody := ""
				if mode == "write" || mode == "copy" {
					wantBody = "paid"
				}
				if response.StatusCode != status || string(body) != wantBody || response.Header.Get("Payment-Receipt") == "" || response.Header.Get("Cache-Control") != policy.want {
					t.Fatalf("status=%d body=%q cache=%q receipt=%q", response.StatusCode, body, response.Header.Get("Cache-Control"), response.Header.Get("Payment-Receipt"))
				}
			})
		}
	}
}
