// Package eip7702 signs InFlow-sponsored external-wallet approval and settlement operations.
package eip7702

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/platform"
	"github.com/inflowpayai/inflow-go/x402"
)

const Delegation = "0x77021100bD87b7008E5E1989d0eB38555d0d0000"
const EntryPoint = "0x0000000071727De22E5E9d8BAf0edAc6f37da032"

type Signer interface {
	Address() string
	ReadContract(context.Context, string, []byte, string, ...any) (any, error)
	// SignMessage applies the Ethereum personal-message prefix to the supplied hash.
	SignMessage(context.Context, []byte) ([]byte, error)
	SignAuthorization(context.Context, types.SetCodeAuthorization) (types.SetCodeAuthorization, error)
}

type Options struct {
	Environment inflow.Environment
	BaseURL     string
	Timeout     time.Duration
	Transport   http.RoundTripper
	Signer      Signer
	// Consent is required because delegation persists even when payment execution fails.
	Consent func(context.Context, types.SetCodeAuthorization) (bool, error)
}

type Extension struct {
	api     *platform.Client
	signer  Signer
	owner   common.Address
	consent func(context.Context, types.SetCodeAuthorization) (bool, error)
}

func New(options Options) (*Extension, error) {
	if options.Signer == nil || options.Consent == nil || !common.IsHexAddress(options.Signer.Address()) {
		return nil, errors.New("EIP-7702 requires an external signer and delegation consent callback")
	}
	api, err := platform.New(inflow.Options{Environment: options.Environment, BaseURL: options.BaseURL, Timeout: options.Timeout, Transport: options.Transport})
	if err != nil {
		return nil, err
	}
	return &Extension{api: api, signer: options.Signer, owner: common.HexToAddress(options.Signer.Address()), consent: options.Consent}, nil
}

func (*Extension) Key() string { return x402.InflowEip7702GasSponsoring }

// EnrichPaymentPayload prepares and signs without broadcasting. Merchant declarations cannot select the preparation URL.
func (e *Extension) EnrichPaymentPayload(ctx context.Context, payload x402.PaymentPayload, required x402.PaymentRequired) (x402.PaymentPayload, error) {
	declaration, advertised := required.Extensions[e.Key()]
	if !advertised || payload.Accepted.Scheme != "exact" || payload.Accepted.Extra["assetTransferMethod"] != "permit2" {
		return payload, nil
	}
	d, err := json.Marshal(declaration)
	if err != nil {
		return payload, invalid("declaration")
	}
	var declarationValue struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	decoder := json.NewDecoder(bytes.NewReader(d))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&declarationValue) != nil || declarationValue.Info.Version != "1" {
		return payload, invalid("declaration")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return payload, invalid("payment")
	}
	var payment x402.PaymentPayload
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	// This JSON was encoded from the same concrete type immediately above.
	_ = decoder.Decode(&payment)
	expected, err := paymentBatch(payment, e.owner)
	if err != nil {
		return payload, err
	}
	allowance, err := e.signer.ReadContract(ctx, expected.asset.Hex(), allowanceABI, "allowance", e.owner, common.HexToAddress(x402.Permit2))
	if err != nil {
		return payload, err
	}
	amount, ok := allowance.(*big.Int)
	if !ok || amount == nil || amount.Sign() < 0 {
		return payload, invalid("Permit2 allowance")
	}
	if amount.Cmp(expected.amount) >= 0 {
		return payload, nil
	}
	raw, err := e.api.Do(ctx, platform.Request{Method: http.MethodPost, Path: "/v1/x402/eip7702/prepare", Body: map[string]any{"paymentPayload": payment, "paymentRequirements": payment.Accepted}})
	if err != nil {
		return payload, err
	}
	prepared, hash, err := validatePreparation(raw.Body, expected, e.owner)
	if err != nil {
		return payload, err
	}
	checkExpiry := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if prepared.ExpiresAt <= time.Now().Unix() || big.NewInt(prepared.ExpiresAt).Cmp(expected.deadline) > 0 {
			return invalid("sponsorship expiry")
		}
		return nil
	}
	if err = checkExpiry(); err != nil {
		return payload, err
	}
	info := x402.SponsorshipInfo{Version: "1", SponsorshipID: prepared.SponsorshipID}
	if a := prepared.authorization; a != nil {
		if a.ChainID != expected.chainID || !sameAddress(a.Address, Delegation) {
			return payload, invalid("delegation authorization")
		}
		authorization := types.SetCodeAuthorization{ChainID: *uint256.NewInt(a.ChainID), Address: common.HexToAddress(Delegation), Nonce: a.Nonce}
		consent, err := e.consent(ctx, authorization)
		if err != nil {
			return payload, err
		}
		if !consent {
			return payload, errors.New("EIP-7702 delegation consent declined")
		}
		if err = checkExpiry(); err != nil {
			return payload, err
		}
		signed, err := e.signer.SignAuthorization(ctx, authorization)
		if err != nil {
			return payload, err
		}
		if signed.ChainID != authorization.ChainID || signed.Address != authorization.Address || signed.Nonce != authorization.Nonce {
			return payload, invalid("changed delegation authorization")
		}
		owner, err := signed.Authority()
		if err != nil || owner != e.owner {
			return payload, invalid("delegation signer")
		}
		signature := make([]byte, 65)
		signed.R.WriteToSlice(signature[:32])
		signed.S.WriteToSlice(signature[32:64])
		signature[64] = signed.V + 27
		info.AuthorizationSignature = "0x" + hex.EncodeToString(signature)
	}
	if err = checkExpiry(); err != nil {
		return payload, err
	}
	signature, err := e.signer.SignMessage(ctx, bytes.Clone(hash))
	if err != nil {
		return payload, err
	}
	if len(signature) != 65 {
		return payload, invalid("operation signature")
	}
	normalized := bytes.Clone(signature)
	if normalized[64] >= 27 {
		normalized[64] -= 27
	}
	key, err := crypto.SigToPub(accounts.TextHash(hash), normalized)
	if err != nil || crypto.PubkeyToAddress(*key) != e.owner {
		return payload, invalid("operation signer")
	}
	if err = checkExpiry(); err != nil {
		return payload, err
	}
	normalized[64] += 27
	info.Signature = "0x" + hex.EncodeToString(normalized)
	if payment.Extensions == nil {
		payment.Extensions = make(map[string]any)
	}
	payment.Extensions[e.Key()] = map[string]any{"info": info}
	return payment, nil
}

func invalid(field string) error   { return errors.New("invalid EIP-7702 " + field) }
func sameAddress(a, b string) bool { return common.IsHexAddress(a) && strings.EqualFold(a, b) }
