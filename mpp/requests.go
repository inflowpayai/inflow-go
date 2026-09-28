package mpp

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

type InflowMethodDetails struct {
	Rail         string `json:"rail,omitempty"`
	InstrumentID string `json:"instrumentId,omitempty"`
}

type ChargeRequest struct {
	Amount        string               `json:"amount"`
	Currency      string               `json:"currency"`
	Recipient     string               `json:"recipient,omitempty"`
	MethodDetails *InflowMethodDetails `json:"methodDetails,omitempty"`
}

type SubscriptionRequest struct {
	ChargeRequest
	PeriodUnit          string  `json:"periodUnit"`
	PeriodCount         int64   `json:"periodCount"`
	SubscriptionExpires string  `json:"subscriptionExpires"`
	ExternalID          *string `json:"externalId,omitempty"`
}

type TempoSplit struct {
	Amount    string  `json:"amount"`
	Recipient string  `json:"recipient"`
	Memo      *string `json:"memo,omitempty"`
}

type TempoMethodDetails struct {
	ChainID        *int64       `json:"chainId,omitempty"`
	FeePayer       *bool        `json:"feePayer,omitempty"`
	Memo           *string      `json:"memo,omitempty"`
	Splits         []TempoSplit `json:"splits,omitempty"`
	SupportedModes []string     `json:"supportedModes,omitempty"`
}

type TempoRequest struct {
	Amount        string              `json:"amount"`
	Currency      string              `json:"currency,omitempty"`
	Recipient     string              `json:"recipient,omitempty"`
	Description   *string             `json:"description,omitempty"`
	ExternalID    *string             `json:"externalId,omitempty"`
	MethodDetails *TempoMethodDetails `json:"methodDetails,omitempty"`
}

type TempoPayload struct {
	Type          string `json:"type"`
	Hash          string `json:"hash,omitempty"`
	Signature     string `json:"signature,omitempty"`
	TransactionID string `json:"transactionId,omitempty"`
}

var (
	decimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	integer = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	guid    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	address = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	bytes32 = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	hex     = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
)

// Validate checks request shape, not capability availability or authorization.
func (r ChargeRequest) Validate() error {
	if !decimal.MatchString(r.Amount) || r.Currency == "" {
		return invalid("request", "decimal amount and currency required")
	}
	if r.Recipient != "" && !guid.MatchString(r.Recipient) {
		return invalid("request", "recipient must be a UUID")
	}
	if d := r.MethodDetails; d != nil {
		if d.Rail != "" && d.Rail != "balance" && d.Rail != "instrument" {
			return invalid("request", "unsupported InFlow rail")
		}
		if d.InstrumentID != "" && !guid.MatchString(d.InstrumentID) {
			return invalid("request", "instrumentId must be a UUID")
		}
	}
	return nil
}

func (r SubscriptionRequest) Validate() error {
	if err := r.ChargeRequest.Validate(); err != nil {
		return err
	}
	if strings.HasPrefix(r.Amount, "-") || !strings.ContainsAny(r.Amount, "123456789") {
		return invalid("request", "subscription amount must be positive")
	}
	if !slices.Contains([]string{"minute", "hour", "day", "week", "month", "quarter", "year"}, r.PeriodUnit) || r.PeriodCount < 1 || r.PeriodCount > 9007199254740991 || (r.PeriodUnit == "minute" && r.PeriodCount < 5) {
		return invalid("request", "invalid subscription period")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.SubscriptionExpires); err != nil {
		return invalid("request", "subscriptionExpires must be RFC 3339")
	}
	if r.ExternalID != nil && (strings.TrimSpace(*r.ExternalID) == "" || len([]rune(*r.ExternalID)) > 128) {
		return invalid("request", "externalId must contain 1 to 128 characters")
	}
	return nil
}

func (r TempoRequest) Validate() error {
	if !integer.MatchString(r.Amount) {
		return invalid("request", "Tempo amount must be a non-negative integer string")
	}
	for _, value := range []string{r.Currency, r.Recipient} {
		if value != "" && !address.MatchString(value) {
			return invalid("request", "invalid Tempo address")
		}
	}
	for _, value := range []*string{r.Description, r.ExternalID} {
		if value != nil && *value == "" {
			return invalid("request", "description and externalId must be non-empty when supplied")
		}
	}
	if d := r.MethodDetails; d != nil {
		if d.Memo != nil && !bytes32.MatchString(*d.Memo) {
			return invalid("request", "memo must be bytes32 hex")
		}
		for _, split := range d.Splits {
			if !integer.MatchString(split.Amount) || !address.MatchString(split.Recipient) || (split.Memo != nil && !bytes32.MatchString(*split.Memo)) {
				return invalid("request", "invalid Tempo split")
			}
		}
		for _, mode := range d.SupportedModes {
			if mode != "pull" && mode != "push" {
				return invalid("request", "unsupported Tempo submission mode")
			}
		}
	}
	return nil
}

// Validate checks the selected proof's fields; it does not verify the proof.
func (p TempoPayload) Validate() error {
	if p.Hash != "" && !hex.MatchString(p.Hash) || p.Signature != "" && !hex.MatchString(p.Signature) {
		return invalid("payload", "proof must be hexadecimal")
	}
	switch p.Type {
	case "hash":
		if p.Hash != "" {
			return nil
		}
	case "transaction", "proof":
		if p.Signature != "" {
			return nil
		}
	}
	return invalid("payload", "hash requires hash; transaction and proof require signature")
}
