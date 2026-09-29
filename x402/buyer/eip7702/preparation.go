package eip7702

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type preparation struct {
	SponsorshipID     string          `json:"sponsorshipId"`
	ChainID           uint64          `json:"chainId"`
	EntryPoint        string          `json:"entryPoint"`
	EntryPointVersion string          `json:"entryPointVersion"`
	Delegation        string          `json:"delegation"`
	Authorization     json.RawMessage `json:"authorization"`
	UserOperation     json.RawMessage `json:"userOperation"`
	UserOperationHash string          `json:"userOperationHash"`
	ExpiresAt         int64           `json:"expiresAt"`
	authorization     *authorization
}
type authorization struct {
	Address string `json:"address"`
	ChainID uint64 `json:"chainId"`
	Nonce   uint64 `json:"nonce"`
}
type operation struct {
	Sender                        string `json:"sender"`
	Nonce                         string `json:"nonce"`
	CallData                      string `json:"callData"`
	CallGasLimit                  string `json:"callGasLimit"`
	VerificationGasLimit          string `json:"verificationGasLimit"`
	PreVerificationGas            string `json:"preVerificationGas"`
	MaxFeePerGas                  string `json:"maxFeePerGas"`
	MaxPriorityFeePerGas          string `json:"maxPriorityFeePerGas"`
	Paymaster                     string `json:"paymaster"`
	PaymasterData                 string `json:"paymasterData"`
	PaymasterVerificationGasLimit string `json:"paymasterVerificationGasLimit"`
	PaymasterPostOpGasLimit       string `json:"paymasterPostOpGasLimit"`
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validatePreparation(data []byte, expected expectedPayment, owner common.Address) (preparation, []byte, error) {
	var p preparation
	if json.Unmarshal(data, &p) != nil {
		return p, nil, invalid("preparation")
	}
	if !uuidPattern.MatchString(p.SponsorshipID) || p.ChainID != expected.chainID || p.EntryPointVersion != "0.7" || !sameAddress(p.EntryPoint, EntryPoint) || !sameAddress(p.Delegation, Delegation) {
		return p, nil, invalid("preparation contracts")
	}
	if len(p.Authorization) > 0 {
		var a struct {
			Address *string `json:"address"`
			ChainID *uint64 `json:"chainId"`
			Nonce   *uint64 `json:"nonce"`
		}
		d := json.NewDecoder(bytes.NewReader(p.Authorization))
		d.DisallowUnknownFields()
		if d.Decode(&a) != nil || a.Address == nil || a.ChainID == nil || a.Nonce == nil || *a.ChainID > 9007199254740991 || *a.Nonce > 9007199254740991 {
			return p, nil, invalid("authorization fields")
		}
		p.authorization = &authorization{*a.Address, *a.ChainID, *a.Nonce}
	}
	var op operation
	d := json.NewDecoder(bytes.NewReader(p.UserOperation))
	d.DisallowUnknownFields()
	if d.Decode(&op) != nil {
		return p, nil, invalid("operation fields")
	}
	callData, ok := hexBytes(op.CallData)
	if !ok || !bytes.Equal(callData, expected.callData) || !sameAddress(op.Sender, owner.Hex()) {
		return p, nil, invalid("operation payment")
	}
	nonce, ok := quantity(op.Nonce, 256)
	if !ok || new(big.Int).Rsh(new(big.Int).Set(nonce), 64).Cmp(big.NewInt(1)) != 0 {
		return p, nil, invalid("operation nonce key")
	}
	callGas, ok := quantity(op.CallGasLimit, 128)
	if !ok || callGas.Sign() == 0 {
		return p, nil, invalid("call gas limit")
	}
	verifyGas, ok := quantity(op.VerificationGasLimit, 128)
	if !ok || verifyGas.Sign() == 0 {
		return p, nil, invalid("verification gas limit")
	}
	if !sameAddress(op.Paymaster, (common.Address{}).Hex()) || op.PaymasterData != "0x" || op.PaymasterVerificationGasLimit != "0x0" || op.PaymasterPostOpGasLimit != "0x0" || op.MaxFeePerGas != "0x0" || op.MaxPriorityFeePerGas != "0x0" || op.PreVerificationGas != "0x0" {
		return p, nil, invalid("bundler sponsorship profile")
	}
	// EntryPoint 0.7 hashes empty initCode and paymasterAndData for this pinned sponsorship profile.
	packed := make([]byte, 8*32)
	copy(packed[12:32], owner[:])
	nonce.FillBytes(packed[32:64])
	copy(packed[64:96], crypto.Keccak256(nil))
	copy(packed[96:128], crypto.Keccak256(callData))
	verifyGas.FillBytes(packed[128:144])
	callGas.FillBytes(packed[144:160])
	copy(packed[224:256], crypto.Keccak256(nil))
	outer := make([]byte, 96)
	copy(outer[:32], crypto.Keccak256(packed))
	copy(outer[44:64], common.HexToAddress(EntryPoint).Bytes())
	new(big.Int).SetUint64(p.ChainID).FillBytes(outer[64:])
	hash := crypto.Keccak256(outer)
	declared, ok := hexBytes(p.UserOperationHash)
	if !ok || !bytes.Equal(hash, declared) {
		return p, nil, invalid("operation hash")
	}
	return p, hash, nil
}
