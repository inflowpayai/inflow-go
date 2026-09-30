package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/mpp"
	b "github.com/inflowpayai/inflow-go/mpp/buyer"
)

func TestProtocol(t *testing.T) {
	for _, tc := range []struct{ version, operation, input, key string }{
		{"1", "mpp.core.decode", `{"value":"e30"}`, "result"},
		{"1", "mpp.core.decode", `{"value":"bad"}`, "error"},
		{"2", "mpp.core.decode", `{"value":"e30"}`, "error"},
		{"1", "not-an-operation", `{}`, "error"},
	} {
		encoded, err := json.Marshal(request{Version: tc.version, Sequence: 3, CaseID: "test", Operation: tc.operation, Input: json.RawMessage(tc.input)})
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := serve(bytes.NewReader(append(encoded, '\n')), &output); err != nil {
			t.Fatal(err)
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response) != 4 || response[tc.key] == nil || string(response["sequence"]) != "3" || string(response["case_id"]) != `"test"` {
			t.Fatal(output.String())
		}
	}
	for _, input := range []string{"bad\n", strings.Repeat("x", 1024*1024+1)} {
		if serve(strings.NewReader(input), io.Discard) == nil {
			t.Fatal("invalid protocol input accepted")
		}
	}
	if serve(strings.NewReader("{}\n"), brokenWriter{}) == nil {
		t.Fatal("write error ignored")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLoopbackAndUnknownErrors(t *testing.T) {
	for _, address := range []string{"https://127.0.0.1:1234", "http://localhost:1234", "http://127.0.0.1:1234/path", "http://user@127.0.0.1:1234", "http://127.0.0.1:1234?q=x", "http://127.0.0.1:1234#x", "http://example.com"} {
		data, err := json.Marshal(map[string]string{"base_url": address})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := platformOptions(data); err == nil {
			t.Fatal(address)
		}
	}
	r := request{Operation: "mpp.buyer.fulfil", Input: json.RawMessage(`{}`)}
	if got := classify(errors.New("payment expired"), r); got.Code != "ADAPTER_ERROR" {
		t.Fatal(got)
	}
	if got := classify(&b.Error{Code: b.InvalidCredential, Cause: &mpp.CodecError{}}, r); got.Code != "invalid-credential" {
		t.Fatal(got)
	}
	if got := classify(&b.Error{Code: b.InvalidResponse}, r); got.Code != "ADAPTER_ERROR" {
		t.Fatal(got)
	}
	if got := classify(&inflow.APIError{Code: "SERVER", Message: "specific", HTTPStatus: 403}, r); got.Code != "SERVER" || got.Message != "specific" || got.Status != 403 {
		t.Fatal(got)
	}
}

func TestMutationGuard(t *testing.T) {
	input := map[string]any{"nested": map[string]any{"amount": "1"}}
	var outcome error
	check := watchInput(input, &outcome)
	check()
	if outcome != nil {
		t.Fatal(outcome)
	}
	input["nested"].(map[string]any)["amount"] = "2"
	check()
	if outcome == nil {
		t.Fatal("mutation not detected")
	}
}

func TestRuntimeUsesPublicHTTP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/mpp/config" || r.Header.Get("X-API-Key") != "test-only-key" {
			t.Error("wrong public call")
		}
		w.WriteHeader(403)
		io.WriteString(w, `{"errors":[{"code":"SELLER_ACCOUNT_REQUIRED","message":"Seller required"}]}`)
	}))
	defer server.Close()
	input, err := json.Marshal(map[string]string{"product": "mpp-seller", "base_url": server.URL, "api_key": "test-only-key"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeRuntime(context.Background(), "runtime.request", input)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.(map[string]any)["code"] != "SELLER_ACCOUNT_REQUIRED" {
		t.Fatal(result, calls)
	}
}
