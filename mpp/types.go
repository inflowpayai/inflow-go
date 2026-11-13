package mpp

import (
	"encoding/json"
	"fmt"
)

const (
	MethodInflow       = "inflow"
	MethodTempo        = "tempo"
	MethodStripe       = "stripe"
	MethodCard         = "card"
	IntentCharge       = "charge"
	IntentSubscription = "subscription"
)

// Challenge preserves the encoded binding fields and display description.
// Upstream's credential echo cannot represent the description.
type Challenge struct {
	ID          string  `json:"id"`
	Realm       string  `json:"realm"`
	Method      string  `json:"method"`
	Intent      string  `json:"intent"`
	Request     string  `json:"request"`
	Expires     *string `json:"expires,omitempty"`
	Description *string `json:"description,omitempty"`
	Digest      *string `json:"digest,omitempty"`
	Opaque      *string `json:"opaque,omitempty"`
}

type Credential struct {
	Challenge Challenge      `json:"challenge"`
	Payload   map[string]any `json:"payload"`
	// Source is optional in the MPP envelope; InFlow-issued credentials include it.
	Source *string `json:"source,omitempty"`
}

type Settlement struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Receipt preserves optional field presence, including an explicitly empty externalId.
type Receipt struct {
	Method              string                     `json:"method"`
	Reference           string                     `json:"reference"`
	Status              string                     `json:"status"`
	Timestamp           string                     `json:"timestamp"`
	ChallengeID         *string                    `json:"challengeId,omitempty"`
	InflowTransactionID *string                    `json:"inflowTransactionId,omitempty"`
	SubscriptionID      *string                    `json:"subscriptionId,omitempty"`
	ExternalID          *string                    `json:"externalId,omitempty"`
	Settlement          *Settlement                `json:"settlement,omitempty"`
	Extensions          map[string]json.RawMessage `json:"-"`
}

type receiptFields Receipt

var receiptKeys = []string{"method", "reference", "status", "timestamp", "challengeId", "inflowTransactionId", "subscriptionId", "externalId", "settlement"}

func (r Receipt) MarshalJSON() ([]byte, error) {
	fields := map[string]any{
		"method":    r.Method,
		"reference": r.Reference,
		"status":    r.Status,
		"timestamp": r.Timestamp,
	}
	for key, value := range map[string]*string{"challengeId": r.ChallengeID, "subscriptionId": r.SubscriptionID, "externalId": r.ExternalID} {
		if value != nil {
			fields[key] = *value
		}
	}
	if r.Settlement != nil {
		fields["settlement"] = r.Settlement
	}
	if r.InflowTransactionID != nil && len(*r.InflowTransactionID) == 36 && guid.MatchString(*r.InflowTransactionID) {
		fields["inflowTransactionId"] = *r.InflowTransactionID
	}
	for key, value := range r.Extensions {
		for _, reserved := range receiptKeys {
			if key == reserved {
				return nil, fmt.Errorf("mpp: receipt extension conflicts with %s", key)
			}
		}
		fields[key] = value
	}
	return json.Marshal(fields)
}

func (r *Receipt) UnmarshalJSON(data []byte) error {
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	var fields receiptFields
	var correlation struct {
		*receiptFields
		InflowTransactionID json.RawMessage `json:"inflowTransactionId"`
	}
	correlation.receiptFields = &fields
	if err := json.Unmarshal(data, &correlation); err != nil {
		return err
	}
	var id string
	if json.Unmarshal(correlation.InflowTransactionID, &id) == nil && len(id) == 36 && guid.MatchString(id) {
		fields.InflowTransactionID = &id
	}
	for _, key := range receiptKeys {
		delete(extra, key)
	}
	fields.Extensions = extra
	*r = Receipt(fields)
	return nil
}

// CodecError describes malformed wire data without including credential contents.
type CodecError struct {
	Artifact string
	Reason   string
}

func (e *CodecError) Error() string { return "mpp: invalid " + e.Artifact + ": " + e.Reason }

func invalid(artifact, reason string) error { return &CodecError{Artifact: artifact, Reason: reason} }
