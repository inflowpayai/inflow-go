// Package seller verifies the InFlow profile of Visa Trusted Agent Protocol.
// Verification recognizes a signing agent, not a buyer or a completed payment.
package seller

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Request struct {
	Method  string
	URL     string
	Headers http.Header
	// Nil means absent; a non-nil empty slice requires signed digest and type fields.
	Body []byte
}

type Facts struct {
	Verified          bool     `json:"verified"`
	KeyID             string   `json:"keyid"`
	Algorithm         string   `json:"algorithm"`
	Intent            string   `json:"intent"`
	Nonce             string   `json:"nonce"`
	Created           int64    `json:"created"`
	Expires           int64    `json:"expires"`
	CoveredComponents []string `json:"coveredComponents"`
}

// KeyResolver returns trusted Ed25519 material for the identifier, or nil if absent.
// Implementations must honor context and support concurrent calls. Untrusted key
// identifiers must not select retrieval URLs.
type KeyResolver interface {
	Resolve(context.Context, string, string) (ed25519.PublicKey, error)
}

// ReplayStore atomically claims a key/nonce pair until expires (Unix seconds).
// Implementations must honor context and support concurrent calls.
type ReplayStore interface {
	Claim(ctx context.Context, keyID, nonce string, expires int64) (bool, error)
}

type Options struct {
	KeyResolver KeyResolver
	ReplayStore ReplayStore
	Clock       func() time.Time
}

type Error struct {
	Code  string
	Cause error
}

func (e *Error) Error() string { return "TAP " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }

type Verifier struct {
	resolver KeyResolver
	store    ReplayStore
	clock    func() time.Time
}

// New performs no network requests. Defaults use Visa's key service and a
// process-local replay store; multiple instances need a shared atomic store.
func New(options Options) *Verifier {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	resolver := options.KeyResolver
	if resolver == nil {
		resolver = NewVisaKeyResolver(KeyResolverOptions{Clock: clock})
	}
	store := options.ReplayStore
	if store == nil {
		store = NewMemoryReplayStore(clock)
	}
	return &Verifier{resolver, store, clock}
}

func (v *Verifier) Verify(ctx context.Context, request Request) (Facts, error) {
	if err := ctx.Err(); err != nil {
		return Facts{}, err
	}
	signature := header(request.Headers, "signature")
	if signature == "" {
		return Facts{}, failure("SIGNATURE_INPUT_INVALID")
	}
	input, err := parseInput(header(request.Headers, "signature-input"))
	if err != nil {
		return Facts{}, err
	}
	required := []string{"@method", "@authority", "@path", "@query"}
	if request.Body != nil {
		required = append(required, "content-digest", "content-type")
	}
	if len(input.components) != len(required) {
		return Facts{}, failure("SIGNATURE_INPUT_INVALID")
	}
	for _, name := range required {
		found := false
		for _, component := range input.components {
			if component == name {
				found = true
			}
		}
		if !found {
			return Facts{}, failure("SIGNATURE_INPUT_INVALID")
		}
	}
	// Check time at verification start, not after potentially slow key retrieval.
	now := v.clock().Unix()
	if input.expires <= input.created || input.expires-input.created > 480 {
		return Facts{}, failure("SIGNATURE_LIFETIME_INVALID")
	}
	if now < input.created {
		return Facts{}, failure("SIGNATURE_NOT_YET_VALID")
	}
	if now >= input.expires {
		return Facts{}, failure("SIGNATURE_EXPIRED")
	}
	u, err := url.Parse(request.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return Facts{}, failure("SIGNATURE_INPUT_INVALID")
	}
	authority := strings.ToLower(u.Host)
	if u.Scheme == "https" {
		authority = strings.TrimSuffix(authority, ":443")
	} else {
		authority = strings.TrimSuffix(authority, ":80")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	values := map[string]string{"@method": request.Method, "@authority": authority, "@path": path, "@query": "?" + u.RawQuery}
	if request.Body != nil {
		contentType := header(request.Headers, "content-type")
		if contentType == "" {
			return Facts{}, failure("SIGNATURE_INPUT_INVALID")
		}
		digest := sha256.Sum256(request.Body)
		expected := "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
		if header(request.Headers, "content-digest") != expected {
			return Facts{}, failure("CONTENT_DIGEST_INVALID")
		}
		values["content-type"], values["content-digest"] = contentType, expected
	}
	var lines []string
	for _, name := range input.components {
		lines = append(lines, `"`+name+`": `+values[name])
	}
	lines = append(lines, `"@signature-params": `+input.canonical)
	base := []byte(strings.Join(lines, "\n"))
	key, err := v.resolver.Resolve(ctx, input.keyID, "ed25519")
	if err != nil {
		return Facts{}, err
	}
	if err := ctx.Err(); err != nil {
		return Facts{}, err
	}
	if key == nil {
		return Facts{}, failure("KEY_NOT_FOUND")
	}
	encoded := signaturePattern.FindStringSubmatch(signature)
	if encoded == nil {
		return Facts{}, failure("SIGNATURE_INPUT_INVALID")
	}
	// The field grammar above has already validated the base64 alphabet and padding.
	sig, _ := base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded[1], "="))
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, base, sig) {
		return Facts{}, failure("SIGNATURE_INVALID")
	}
	claimed, err := v.store.Claim(ctx, input.keyID, input.nonce, input.expires)
	if err != nil {
		return Facts{}, err
	}
	if err := ctx.Err(); err != nil {
		return Facts{}, err
	}
	if !claimed {
		return Facts{}, failure("NONCE_REPLAYED")
	}
	intent := "browse"
	if input.tag == "agent-payer-auth" {
		intent = "pay"
	}
	return Facts{true, input.keyID, "ed25519", intent, input.nonce, input.created, input.expires, input.components}, nil
}

// WithVerified calls next only after verification and the replay claim succeed.
// The application owns HTTP status, body reading, effective URL and response policy.
func (v *Verifier) WithVerified(ctx context.Context, request Request, next func(Facts) error) error {
	facts, err := v.Verify(ctx, request)
	if err != nil {
		return err
	}
	return next(facts)
}

func header(headers http.Header, name string) string {
	value := ""
	found := false
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		if found || len(values) != 1 {
			return ""
		}
		found, value = true, values[0]
	}
	return value
}

func failure(code string) error { return &Error{Code: code} }
