package x402

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
)

const (
	PaymentIdentifier      = "payment-identifier"
	DefaultPaymentIDPrefix = "pay_"
	PaymentIDMinLength     = 16
	PaymentIDMaxLength     = 128
)

var identifierPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func ValidatePaymentID(id string) bool {
	return len(id) >= PaymentIDMinLength && len(id) <= PaymentIDMaxLength && identifierPattern.MatchString(id)
}

// GeneratePaymentID appends 32 random hexadecimal characters to prefix.
func GeneratePaymentID(prefix string) (string, error) {
	if len(prefix) > PaymentIDMaxLength-32 || (prefix != "" && !identifierPattern.MatchString(prefix)) {
		return "", errors.New("invalid payment identifier prefix")
	}
	var random [16]byte
	rand.Read(random[:])
	return prefix + hex.EncodeToString(random[:]), nil
}

// IdentifierDeclaration retains unknown information and schema fields as JSON.
type IdentifierDeclaration struct {
	Info   map[string]json.RawMessage `json:"info"`
	Schema json.RawMessage            `json:"schema"`
}

const identifierSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"id":{"type":"string","minLength":16,"maxLength":128,"pattern":"^[a-zA-Z0-9_-]+$"},"required":{"type":"boolean"}},"required":["required"]}`

func DeclarePaymentIdentifier() IdentifierDeclaration {
	return IdentifierDeclaration{Info: map[string]json.RawMessage{"required": json.RawMessage("false")}, Schema: json.RawMessage(identifierSchema)}
}

// ReadPaymentIdentifier accepts decoded JSON or an IdentifierDeclaration. Malformed declarations return nil.
func ReadPaymentIdentifier(value any) *IdentifierDeclaration {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil {
		return nil
	}
	declaration := IdentifierDeclaration{Schema: fields["schema"]}
	if json.Unmarshal(fields["info"], &declaration.Info) != nil {
		return nil
	}
	var required *bool
	if json.Unmarshal(declaration.Info["required"], &required) != nil || required == nil {
		return nil
	}
	var schema map[string]any
	if json.Unmarshal(declaration.Schema, &schema) != nil {
		return nil
	}
	properties := object(schema["properties"])
	id := object(properties["id"])
	requiredList, _ := schema["required"].([]any)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["type"] != "object" ||
		id["type"] != "string" || id["minLength"] != float64(16) || id["maxLength"] != float64(128) || id["pattern"] != identifierPattern.String() ||
		object(properties["required"])["type"] != "boolean" || len(requiredList) != 1 || requiredList[0] != "required" {
		return nil
	}
	return &declaration
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

// PaymentIdentifierEntry returns an independent copy with the supplied identifier, or nil for invalid input.
func PaymentIdentifierEntry(declaration any, id string) *IdentifierDeclaration {
	if !ValidatePaymentID(id) {
		return nil
	}
	entry := ReadPaymentIdentifier(declaration)
	if entry == nil {
		return nil
	}
	entry.Info["id"], _ = json.Marshal(id)
	return entry
}
