package seller

import (
	"regexp"
	"strconv"
	"strings"
)

var inputPattern = regexp.MustCompile(`^ *sig2=\( *("[a-z@-]+"(?: +"[a-z@-]+")*) *\)([^\r\n]*)$`)
var parameterPattern = regexp.MustCompile("^; *(created|expires|keyid|alg|nonce|tag)(?:=(\"(?:[\\x20-\\x21\\x23-\\x5b\\x5d-\\x7e]|\\\\[\"\\\\])*\"|-?\\d{1,12}\\.\\d{1,3}|-?\\d{1,15}|\\?[01]|:(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}(?:==)?|[A-Za-z0-9+/]{3}=?)?:|[A-Za-z*][A-Za-z0-9!#$%&'*+.^_`|~:/-]*))?")
var signaturePattern = regexp.MustCompile(`^ *sig2=:((?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}(?:==)?|[A-Za-z0-9+/]{3}=?)?):[ \t]*$`)
var integerPattern = regexp.MustCompile(`^-?\d+$`)

type parsedInput struct {
	components                   []string
	created, expires             int64
	keyID, nonce, tag, canonical string
}

type parameter struct {
	text    string
	integer int64
	kind    byte
}

func parseInput(value string) (parsedInput, error) {
	bad := func() (parsedInput, error) { return parsedInput{}, failure("SIGNATURE_INPUT_INVALID") }
	match := inputPattern.FindStringSubmatch(value)
	if match == nil {
		return bad()
	}
	var input parsedInput
	seen := map[string]bool{}
	for _, component := range strings.Fields(match[1]) {
		name := strings.Trim(component, `"`)
		if seen[name] {
			return bad()
		}
		seen[name] = true
		input.components = append(input.components, name)
	}
	parameters := map[string]parameter{}
	var order []string
	remaining := strings.TrimRight(match[2], " \t")
	for remaining != "" {
		part := parameterPattern.FindStringSubmatch(remaining)
		if part == nil {
			return bad()
		}
		remaining = remaining[len(part[0]):]
		if remaining != "" && !strings.HasPrefix(remaining, ";") {
			return bad()
		}
		name, encoded := part[1], part[2]
		item := parameter{}
		if strings.HasPrefix(encoded, `"`) {
			item.kind = 's'
			item.text = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(encoded[1 : len(encoded)-1])
		} else if integerPattern.MatchString(encoded) {
			item.kind = 'i'
			// Structured Fields bounds integers to fifteen digits, within int64.
			item.integer, _ = strconv.ParseInt(encoded, 10, 64)
		}
		if _, exists := parameters[name]; !exists {
			order = append(order, name)
		}
		// RFC 8941 preserves first position and uses the last value and its type.
		parameters[name] = item
	}
	if parameters["created"].kind != 'i' || parameters["expires"].kind != 'i' {
		return bad()
	}
	for _, name := range []string{"keyid", "alg", "nonce", "tag"} {
		if parameters[name].kind != 's' || parameters[name].text == "" {
			return bad()
		}
	}
	algorithm, tag := parameters["alg"].text, parameters["tag"].text
	if (algorithm != "ed25519" && algorithm != "Ed25519") || (tag != "agent-browser-auth" && tag != "agent-payer-auth") {
		return bad()
	}
	input.created, input.expires = parameters["created"].integer, parameters["expires"].integer
	input.keyID, input.nonce, input.tag = parameters["keyid"].text, parameters["nonce"].text, tag
	var components []string
	for _, name := range input.components {
		components = append(components, `"`+name+`"`)
	}
	input.canonical = "(" + strings.Join(components, " ") + ")"
	for _, name := range order {
		item := parameters[name]
		encoded := strconv.FormatInt(item.integer, 10)
		if item.kind == 's' {
			encoded = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(item.text) + `"`
		}
		input.canonical += ";" + name + "=" + encoded
	}
	return input, nil
}
