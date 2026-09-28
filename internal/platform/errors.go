package platform

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	inflow "github.com/inflowpayai/inflow-go"
)

func responseError(path string, status int, headers http.Header, data []byte, secrets ...string) *inflow.APIError {
	var body any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil || !json.Valid(data) {
		body = string(data)
	}
	body = redact(body, secrets)
	code, message := "UNEXPECTED_ERROR", "request failed"
	if object, ok := body.(map[string]any); ok {
		code = text(object, "code", code)
		message = text(object, "message", message)
		if entries, ok := object["errors"].([]any); ok && len(entries) > 0 {
			if first, ok := entries[0].(map[string]any); ok {
				code = text(first, "code", code)
				message = text(first, "message", message)
			}
		}
		message = text(object, "detail", message)
	}
	safe := make(http.Header)
	for name, values := range headers {
		if sensitive(name) {
			continue
		}
		for _, value := range values {
			safe.Add(name, redactText(value, secrets))
		}
	}
	return &inflow.APIError{Code: code, Message: message, HTTPStatus: status, Endpoint: path, RequestID: safe.Get("X-Request-ID"), Headers: safe, Body: body}
}

func text(object map[string]any, key, fallback string) string {
	if value, ok := object[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

var keySeparators = strings.NewReplacer("-", "", "_", "")

func sensitive(name string) bool {
	switch strings.ToLower(keySeparators.Replace(name)) {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "xapikey", "apikey", "accesstoken", "refreshtoken", "privatekey", "secretkey", "password", "credential", "signature", "paymentsignature", "xpayment":
		return true
	}
	return false
}

func redact(value any, secrets []string) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if sensitive(key) {
				v[key] = "[REDACTED]"
			} else {
				v[key] = redact(item, secrets)
			}
		}
	case []any:
		for i, item := range v {
			v[i] = redact(item, secrets)
		}
	case string:
		return redactText(v, secrets)
	}
	return value
}

func redactText(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}
