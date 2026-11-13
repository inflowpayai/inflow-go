// Package mppwire adapts upstream MPP values to the platform's encoded-request JSON contract.
package mppwire

import (
	"errors"
	"strings"

	"github.com/inflowpayai/inflow-go/mpp"
	foundation "github.com/tempoxyz/mpp-go/pkg/mpp"
)

type Challenge = mpp.Challenge
type Credential = mpp.Credential

func FromCredential(value foundation.Credential) (Credential, error) {
	if value.Challenge.Header != "" && !strings.EqualFold(value.Challenge.Header, "Authorization") {
		return Credential{}, errors.New("MPP Payment-Authorization challenges are not supported by the platform integration")
	}
	header, err := foundation.FormatCredential(&value)
	if err != nil {
		return Credential{}, err
	}
	return mpp.DecodeCredential(strings.TrimPrefix(header, "Payment "))
}

func FromChallenge(value foundation.Challenge) (Challenge, error) {
	header, err := foundation.FormatChallenge(&value)
	if err != nil {
		return Challenge{}, err
	}
	return mpp.ParseChallenge(header)
}

func ToChallenge(value Challenge) (foundation.Challenge, error) {
	header, err := mpp.RenderChallenge(value)
	if err != nil {
		return foundation.Challenge{}, err
	}
	challenge, err := foundation.ParseChallenge(header)
	if err != nil {
		return foundation.Challenge{}, err
	}
	return *challenge, nil
}

func ToCredential(value Credential) (foundation.Credential, error) {
	encoded, err := mpp.EncodeCredential(value)
	if err != nil {
		return foundation.Credential{}, err
	}
	credential, err := foundation.ParseCredential("Payment " + encoded)
	if err != nil {
		return foundation.Credential{}, err
	}
	return *credential, nil
}

func ToReceipt(value mpp.Receipt) (*foundation.Receipt, error) {
	encoded, err := mpp.EncodeReceipt(value)
	if err != nil {
		return nil, err
	}
	// Upstream cannot retain an explicitly empty externalId; keep that limitation
	// at this adapter boundary rather than changing InFlow's receipt representation.
	return foundation.ParseReceipt(encoded)
}
