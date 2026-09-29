package seller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/mpp"
)

type Validation struct {
	Success    bool                       `json:"success"`
	Challenge  mpp.Challenge              `json:"challenge"`
	Credential mpp.Credential             `json:"credential"`
	Details    map[string]json.RawMessage `json:"details"`
	Intent     string                     `json:"intent"`
	Method     string                     `json:"method"`
	Request    map[string]json.RawMessage `json:"request"`
	Source     *string                    `json:"source,omitempty"`
	Problem    json.RawMessage            `json:"problem,omitempty"`
}

// Validate asks the platform to check a credential without consuming payment.
// HTTP integrations must first verify the challenge's provenance and route binding.
func (c *Client) Validate(ctx context.Context, credential mpp.Credential) (Validation, error) {
	if err := payloadValid(credential); err != nil {
		return Validation{}, err
	}
	if err := c.Load(ctx); err != nil {
		return Validation{}, err
	}
	raw, err := c.api.Do(ctx, platform.Request{Method: "POST", Path: "/v1/mpp/validate", Body: credentialBody(credential), Retries: 3})
	if err != nil {
		return Validation{}, err
	}
	var value Validation
	decoder := json.NewDecoder(bytes.NewReader(raw.Body))
	decoder.UseNumber()
	err = decoder.Decode(&value)
	if err != nil {
		return Validation{}, rejection(nil)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Validation{}, rejection(nil)
	}
	if !value.Success {
		return Validation{}, rejection(value.Problem)
	}
	// Compare exact credential values without binary64 conversion of proof payloads.
	want, _ := json.Marshal(credential)
	got, _ := json.Marshal(value.Credential)
	if !reflect.DeepEqual(value.Challenge, credential.Challenge) || string(want) != string(got) || value.Method != credential.Challenge.Method || value.Intent != credential.Challenge.Intent || !reflect.DeepEqual(value.Source, credential.Source) || value.Request == nil {
		return Validation{}, rejection(nil)
	}
	return value, nil
}

// Broadcast performs the terminal platform operation. Supply the same key when
// explicitly retrying an uncertain outcome; an empty key generates one if enabled.
func (c *Client) Broadcast(ctx context.Context, credential mpp.Credential, idempotencyKey string) (mpp.Receipt, error) {
	if err := payloadValid(credential); err != nil {
		return mpp.Receipt{}, err
	}
	config, err := c.config(ctx)
	if err != nil {
		return mpp.Receipt{}, err
	}
	headers := make(http.Header)
	retries := 0
	if config.FeatureFlags.IdempotencyKeyEnabled {
		if idempotencyKey == "" {
			var id [16]byte
			rand.Read(id[:])
			id[6] = (id[6] & 15) | 64
			id[8] = (id[8] & 63) | 128
			idempotencyKey = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
		}
		headers.Set("Idempotency-Key", idempotencyKey)
		retries = 3
	}
	raw, err := c.api.Do(ctx, platform.Request{Method: "POST", Path: "/v1/mpp/broadcast", Body: credentialBody(credential), Headers: headers, Retries: retries})
	if err != nil {
		return mpp.Receipt{}, err
	}
	value, err := platform.Decode[struct {
		Receipt *mpp.Receipt    `json:"receipt"`
		Problem json.RawMessage `json:"problem"`
	}](raw)
	if err != nil {
		return mpp.Receipt{}, rejection(nil)
	}
	if value.Receipt == nil {
		return mpp.Receipt{}, rejection(value.Problem)
	}
	if _, err := mpp.EncodeReceipt(*value.Receipt); err != nil {
		return mpp.Receipt{}, rejection(value.Problem)
	}
	return *value.Receipt, nil
}

func (c *Client) Verify(ctx context.Context, credential mpp.Credential) (mpp.Receipt, error) {
	if err := c.Load(ctx); err != nil {
		return mpp.Receipt{}, err
	}
	if _, err := c.Validate(ctx, credential); err != nil {
		return mpp.Receipt{}, err
	}
	return c.Broadcast(ctx, credential, "")
}

func credentialBody(credential mpp.Credential) any {
	return struct {
		Credential mpp.Credential `json:"credential"`
	}{credential}
}
