package mpp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const maxEncodedSize = 64 * 1024

// Canonicalize sorts object keys by UTF-16 and omits null object members, matching
// InFlow's request encoding. Array nulls remain null. Numbers use binary64 JSON
// semantics; monetary values must be decimal strings, not floating-point numbers.
func Canonicalize(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, invalid("value", "not JSON encodable")
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, invalid("value", "not a finite JSON value")
	}
	var out bytes.Buffer
	writeCanonical(&out, normalized)
	return out.Bytes(), nil
}

func writeCanonical(out *bytes.Buffer, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key, item := range v {
			if item != nil {
				keys = append(keys, key)
			}
		}
		slices.SortFunc(keys, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, key)
			out.WriteByte(':')
			writeCanonical(out, v[key])
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			writeCanonical(out, item)
		}
		out.WriteByte(']')
	case string:
		writeString(out, v)
	case float64:
		if v == 0 {
			out.WriteByte('0')
			return
		}
		format := byte('f')
		if math.Abs(v) < 1e-6 || math.Abs(v) >= 1e21 {
			format = 'e'
		}
		number := strconv.FormatFloat(v, format, -1, 64)
		number = strings.ReplaceAll(strings.ReplaceAll(number, "e-0", "e-"), "e+0", "e+")
		out.WriteString(number)
	case bool:
		out.WriteString(strconv.FormatBool(v))
	default:
		out.WriteString("null")
	}
}

func writeString(out *bytes.Buffer, value string) {
	const hex = "0123456789abcdef"
	out.WriteByte('"')
	for _, ch := range value {
		switch ch {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(ch)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if ch < 32 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[ch>>4])
				out.WriteByte(hex[ch&15])
			} else {
				out.WriteRune(ch)
			}
		}
	}
	out.WriteByte('"')
}

func Encode(value any) (string, error) {
	data, err := Canonicalize(value)
	if err != nil {
		return "", err
	}
	if base64.RawURLEncoding.EncodedLen(len(data)) > maxEncodedSize {
		return "", invalid("value", "exceeds 64 KiB encoded limit")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Decode accepts padded or unpadded base64url JSON. Numbers are json.Number to
// avoid losing precision while forwarding opaque payment payloads.
func Decode(value string) (any, error) {
	data, err := decodeBytes(value, "value")
	if err != nil {
		return nil, err
	}
	var result any
	if err := decodeJSON(data, &result); err != nil {
		return nil, invalid("value", "not valid JSON")
	}
	return result, nil
}

func decodeBytes(value, artifact string) ([]byte, error) {
	if len(value) > maxEncodedSize {
		return nil, invalid(artifact, "exceeds 64 KiB encoded limit")
	}
	if strings.ContainsAny(value, "\r\n") {
		return nil, invalid(artifact, "not base64url")
	}
	encoding := base64.RawURLEncoding
	if strings.HasSuffix(value, "=") {
		encoding = base64.URLEncoding
	}
	data, err := encoding.Strict().DecodeString(value)
	if err != nil {
		return nil, invalid(artifact, "not base64url")
	}
	if !utf8.Valid(data) {
		return nil, invalid(artifact, "not valid JSON")
	}
	return data, nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return invalid("value", "expected one JSON value")
	}
	return nil
}

func DecodeCredential(value string) (Credential, error) {
	var result Credential
	data, err := decodeBytes(value, "credential")
	if err != nil {
		return result, err
	}
	if err := decodeJSON(data, &result); err != nil {
		return Credential{}, invalid("credential", "invalid field type")
	}
	if err := validateChallenge(result.Challenge); err != nil {
		return Credential{}, invalid("credential", "missing challenge fields")
	}
	if result.Payload == nil {
		return Credential{}, invalid("credential", "payload must be an object")
	}
	return result, nil
}

func EncodeCredential(value Credential) (string, error) {
	if err := validateChallenge(value.Challenge); err != nil {
		return "", err
	}
	if value.Payload == nil {
		return "", invalid("credential", "payload must be an object")
	}
	// Credentials echo signed values, including null payload members; they are not
	// request templates to normalize with Canonicalize.
	data, err := json.Marshal(value)
	if err != nil {
		return "", invalid("credential", "not JSON encodable")
	}
	if base64.RawURLEncoding.EncodedLen(len(data)) > maxEncodedSize {
		return "", invalid("credential", "exceeds 64 KiB encoded limit")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func DecodeReceipt(value string) (Receipt, error) {
	var result Receipt
	data, err := decodeBytes(value, "receipt")
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Receipt{}, invalid("receipt", "invalid field type")
	}
	if err := validateReceipt(result); err != nil {
		return Receipt{}, err
	}
	return result, nil
}

func EncodeReceipt(value Receipt) (string, error) {
	if err := validateReceipt(value); err != nil {
		return "", err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", invalid("receipt", "not JSON encodable")
	}
	if base64.RawURLEncoding.EncodedLen(len(data)) > maxEncodedSize {
		return "", invalid("receipt", "exceeds 64 KiB encoded limit")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func validateReceipt(r Receipt) error {
	if r.Method == "" || r.Reference == "" || r.Status != "success" {
		return invalid("receipt", "method, reference, and success status required")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Timestamp); err != nil {
		return invalid("receipt", "timestamp must be RFC 3339")
	}
	if (r.ChallengeID != nil && *r.ChallengeID == "") || (r.SubscriptionID != nil && *r.SubscriptionID == "") {
		return invalid("receipt", "identifiers must be non-empty when supplied")
	}
	if r.Settlement != nil && (r.Settlement.Amount == "" || r.Settlement.Currency == "") {
		return invalid("receipt", "settlement amount and currency required")
	}
	return nil
}
