package mpp

import (
	"strings"

	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

// ParseChallenges accepts repeated or combined WWW-Authenticate header values.
// Non-Payment authentication schemes are left to the application's auth handler.
func ParseChallenges(headers []string) ([]Challenge, error) {
	result := make([]Challenge, 0)
	for _, header := range headers {
		if len(header) > maxEncodedSize || !safeHeader(header) {
			return nil, invalid("challenge header", "unsafe or oversized header")
		}
		for _, part := range foundation.SplitAuthenticate(header) {
			name := part
			if end := strings.IndexAny(part, " \t"); end >= 0 {
				name = part[:end]
			}
			if !strings.EqualFold(name, "Payment") {
				continue
			}
			challenge, err := ParseChallenge(part)
			if err != nil {
				return nil, err
			}
			result = append(result, challenge)
		}
	}
	return result, nil
}

func ParseChallenge(header string) (Challenge, error) {
	var result Challenge
	if len(header) > maxEncodedSize || !safeHeader(header) {
		return result, invalid("challenge header", "unsafe or oversized header")
	}
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Payment") {
		return result, invalid("challenge header", "expected Payment scheme")
	}
	fields, err := parseParams(rest)
	if err != nil {
		return result, err
	}
	result = Challenge{ID: fields["id"], Realm: fields["realm"], Method: fields["method"], Intent: fields["intent"], Request: fields["request"]}
	for key, target := range map[string]**string{"expires": &result.Expires, "description": &result.Description, "digest": &result.Digest, "opaque": &result.Opaque} {
		if value, exists := fields[key]; exists {
			*target = &value
		}
	}
	if err := validateChallenge(result); err != nil {
		return Challenge{}, err
	}
	return result, nil
}

func RenderChallenge(c Challenge) (string, error) {
	header, err := foundation.FormatAuthenticateStrict(&foundation.Challenge{ID: c.ID, Method: c.Method, Intent: c.Intent, RequestB64: c.Request}, c.Realm)
	if err != nil {
		return "", invalid("challenge header", "unsafe field")
	}
	if err := validateChallenge(c); err != nil {
		return "", err
	}
	for _, item := range []struct {
		key   string
		value *string
	}{{"expires", c.Expires}, {"description", c.Description}, {"digest", c.Digest}, {"opaque", c.Opaque}} {
		if item.value != nil {
			header += ", " + item.key + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(*item.value) + `"`
		}
	}
	if len(header) > maxEncodedSize {
		return "", invalid("challenge header", "exceeds 64 KiB limit")
	}
	return header, nil
}

func validateChallenge(c Challenge) error {
	for _, value := range []string{c.ID, c.Realm, c.Method, c.Intent, c.Request} {
		if value == "" || !safeHeader(value) {
			return invalid("challenge", "required fields must be non-empty and header-safe")
		}
	}
	for _, value := range []*string{c.Expires, c.Description, c.Digest, c.Opaque} {
		if value != nil && !safeHeader(*value) {
			return invalid("challenge", "optional fields must be header-safe")
		}
	}
	return nil
}

func safeHeader(value string) bool {
	for _, ch := range value {
		if (ch < 32 && ch != '\t') || ch == 127 {
			return false
		}
	}
	return true
}

func token(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch))
}

func parseParams(input string) (map[string]string, error) {
	fields := make(map[string]string)
	for {
		input = strings.TrimSpace(input)
		i := 0
		for i < len(input) && token(input[i]) {
			i++
		}
		if i == 0 {
			return nil, invalid("challenge header", "invalid parameter name")
		}
		key := strings.ToLower(input[:i])
		input = strings.TrimSpace(input[i:])
		if !strings.HasPrefix(input, "=") {
			return nil, invalid("challenge header", "missing parameter value")
		}
		input = strings.TrimSpace(input[1:])
		var value strings.Builder
		if strings.HasPrefix(input, `"`) {
			input = input[1:]
			closed := false
			for len(input) > 0 {
				ch := input[0]
				input = input[1:]
				if ch == '"' {
					closed = true
					break
				}
				if ch == '\\' {
					if input == "" {
						break
					}
					ch = input[0]
					input = input[1:]
				}
				value.WriteByte(ch)
			}
			if !closed {
				return nil, invalid("challenge header", "unterminated quoted value")
			}
		} else {
			i = 0
			for i < len(input) && token(input[i]) {
				i++
			}
			if i == 0 {
				return nil, invalid("challenge header", "empty unquoted value")
			}
			value.WriteString(input[:i])
			input = input[i:]
		}
		if _, exists := fields[key]; exists {
			return nil, invalid("challenge header", "duplicate parameter")
		}
		fields[key] = value.String()
		input = strings.TrimSpace(input)
		if input == "" {
			break
		}
		if input[0] != ',' {
			return nil, invalid("challenge header", "missing parameter separator")
		}
		input = strings.TrimSpace(input[1:])
		if input == "" {
			return nil, invalid("challenge header", "trailing separator")
		}
	}
	return fields, nil
}
