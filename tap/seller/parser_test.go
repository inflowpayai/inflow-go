package seller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func TestStructuredParameters(t *testing.T) {
	original := signedRequest(nil).Headers.Get("Signature-Input")
	for _, suffix := range []string{";extra=1", ";created=1.5", ";nonce=?1", ";alg=ed25519", ";tag", ";nonce=\"bad\\q\"", ";created=1234567890123456", ";created=2 nope", ";nope=1"} {
		_, err := parseInput(original + suffix)
		code(t, err, "SIGNATURE_INPUT_INVALID")
	}
	for _, old := range []string{`keyid="test"`, `alg="ed25519"`, `nonce="nonce"`, `tag="agent-browser-auth"`} {
		_, err := parseInput(strings.Replace(original, ";"+old, "", 1))
		code(t, err, "SIGNATURE_INPUT_INVALID")
	}
	for _, value := range []string{
		strings.Replace(original, `"@method"`, `"@method" "@method"`, 1),
		strings.Replace(original, `alg="ed25519"`, `alg="rsa"`, 1),
		strings.Replace(original, `tag="agent-browser-auth"`, `tag="other"`, 1),
	} {
		_, err := parseInput(value)
		code(t, err, "SIGNATURE_INPUT_INVALID")
	}
	for _, previous := range []string{"?1", ":YQ==:", ":YQ:", "123.5", "token", "\"obsolete\"", "-0"} {
		value := strings.Replace(original, ";created=999", ";created="+previous+";created=000999", 1)
		parsed, err := parseInput(value)
		if err != nil || "sig2="+parsed.canonical != original {
			t.Fatalf("%s: %+v %v", previous, parsed, err)
		}
	}
}

func TestCanonicalSignedVariants(t *testing.T) {
	for _, raw := range []string{
		`("@query" "@path" "@authority" "@method");nonce="escaped\\value\"quote";tag="agent-payer-auth";keyid="test";created=999;expires=1100;alg="Ed25519"`,
		`("@query" "@path" "@authority" "@method");nonce="nonce";tag="agent-payer-auth";keyid="test";created=999;expires=1100;alg="ed25519"`,
	} {
		r := signedRequest(nil)
		base := `"@query": ?a=1&b=%2f` + "\n" + `"@path": /path%20value` + "\n" + `"@authority": example.com` + "\n" + `"@method": POST` + "\n" + `"@signature-params": ` + raw
		r.Headers.Set("Signature", " sig2=:"+base64.RawStdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(base)))+": \t")
		r.Headers.Set("Signature-Input", " sig2="+strings.Replace(raw, ";created=999", "; created=?1;created=000999", 1)+" \t")
		v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(testResolver)})
		facts, err := v.Verify(context.Background(), r)
		if err != nil || facts.Intent != "pay" {
			t.Fatalf("%+v %v", facts, err)
		}
	}
}

func TestURLComponents(t *testing.T) {
	for _, u := range []string{"https://EXAMPLE.COM:443", "http://EXAMPLE.COM:80", "https://example.com:8443"} {
		authority := "example.com"
		if strings.Contains(u, "8443") {
			authority += ":8443"
		}
		r := signedRequest(nil)
		r.URL = u
		raw := strings.TrimPrefix(r.Headers.Get("Signature-Input"), "sig2=")
		base := `"@method": POST` + "\n" + `"@authority": ` + authority + "\n" + `"@path": /` + "\n" + `"@query": ?` + "\n" + `"@signature-params": ` + raw
		r.Headers.Set("Signature", "sig2=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(base)))+":")
		v := New(Options{Clock: fixedClock, KeyResolver: resolverFunc(testResolver)})
		if _, err := v.Verify(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}
