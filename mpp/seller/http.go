package seller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/inflowpayai/inflow-go/mpp"
)

type Route struct {
	Realm     string
	SecretKey string
	Offers    []Offer
	// Zero selects five minutes. The signing key is separate from the platform API key.
	Lifetime time.Duration
	Opaque   *string
	// CanOffer selects advertised offers; it does not revoke issued credentials.
	CanOffer func(*http.Request, Offer) (bool, error)
}

// Protect validates and broadcasts payment before running next. It does not
// buffer the handler response or undo payment when the handler fails.
func (c *Client) Protect(route Route, next http.Handler) (http.Handler, error) {
	if route.Realm == "" || route.SecretKey == "" || len(route.Offers) == 0 || route.Lifetime < 0 || next == nil {
		return nil, errors.New("MPP route requires realm, secret key, offers, handler, and non-negative lifetime")
	}
	if route.Lifetime == 0 {
		route.Lifetime = 5 * time.Minute
	}
	// Capture values, not caller-owned request pointers or nested method details.
	raw, _ := json.Marshal(route.Offers)
	var offers []Offer
	_ = json.Unmarshal(raw, &offers)
	route.Offers = offers
	if route.Opaque != nil {
		value := *route.Opaque
		route.Opaque = &value
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.serve(w, r, route, next) }), nil
}

func (c *Client) serve(w http.ResponseWriter, r *http.Request, route Route, next http.Handler) {
	prepared := make([]PreparedOffer, len(route.Offers))
	for i, offer := range route.Offers {
		var err error
		prepared[i], err = c.Prepare(r.Context(), offer)
		if err != nil {
			http.Error(w, "Payment configuration unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	credential, err := paymentCredential(r.Header.Values("Authorization"))
	if err == nil && credential != nil {
		for _, offer := range prepared {
			if !matches(route, offer, *credential) {
				continue
			}
			receipt, err := c.Verify(r.Context(), *credential)
			if err != nil {
				break
			}
			encoded, _ := mpp.EncodeReceipt(receipt)
			w.Header().Set("Payment-Receipt", encoded)
			next.ServeHTTP(w, r)
			return
		}
	}
	var headers []string
	for i, offer := range route.Offers {
		if route.CanOffer != nil {
			// Give the hook its own values so it cannot alter the protected price.
			raw, _ := json.Marshal(offer)
			var copy Offer
			_ = json.Unmarshal(raw, &copy)
			allowed, err := route.CanOffer(r, copy)
			if err != nil {
				http.Error(w, "Payment offer unavailable", http.StatusInternalServerError)
				return
			}
			if !allowed {
				continue
			}
		}
		expires := time.Now().UTC().Add(route.Lifetime).Format(time.RFC3339Nano)
		challenge := mpp.Challenge{Realm: route.Realm, Method: prepared[i].Method, Intent: prepared[i].Intent, Request: prepared[i].Request, Expires: &expires, Opaque: route.Opaque}
		challenge.ID = challengeID(challenge, route.SecretKey)
		header, err := mpp.RenderChallenge(challenge)
		if err != nil {
			http.Error(w, "Payment offer unavailable", http.StatusInternalServerError)
			return
		}
		headers = append(headers, header)
	}
	if len(headers) == 0 {
		http.Error(w, "No payment offers available", http.StatusServiceUnavailable)
		return
	}
	for _, header := range headers {
		w.Header().Add("WWW-Authenticate", header)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "Payment required", http.StatusPaymentRequired)
}

func paymentCredential(headers []string) (*mpp.Credential, error) {
	var result *mpp.Credential
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			fields := strings.Fields(part)
			if len(fields) == 0 || !strings.EqualFold(fields[0], "Payment") {
				continue
			}
			if len(fields) != 2 || result != nil {
				return nil, errors.New("invalid Payment authorization")
			}
			value, err := mpp.DecodeCredential(fields[1])
			if err != nil {
				return nil, err
			}
			result = &value
		}
	}
	return result, nil
}

func matches(route Route, offer PreparedOffer, credential mpp.Credential) bool {
	c := credential.Challenge
	if c.Method != offer.Method || c.Intent != offer.Intent || c.Realm != route.Realm || !reflect.DeepEqual(c.Opaque, route.Opaque) || c.Expires == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, *c.Expires)
	if err != nil || !time.Now().Before(expires) {
		return false
	}
	if !hmac.Equal([]byte(c.ID), []byte(challengeID(c, route.SecretKey))) {
		return false
	}
	actual, err := requestObject(c.Request)
	if err != nil {
		return false
	}
	expected, err := requestObject(offer.Request)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(actual, expected)
}

func challengeID(c mpp.Challenge, secret string) string {
	value := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	// Seven MPP binding slots. Preserve the encoded request and opaque bytes;
	// upstream map conversion would re-encode them before verification.
	input := strings.Join([]string{c.Realm, c.Method, c.Intent, c.Request, value(c.Expires), value(c.Digest), value(c.Opaque)}, "|")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
