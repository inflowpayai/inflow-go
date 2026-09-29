package eip7702

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/inflowpayai/inflow-go/x402"
	"github.com/x402-foundation/x402/go/v2/mechanisms/evm"
)

var allowanceABI = []byte(`[{"name":"allowance","type":"function","inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"outputs":[{"type":"uint256"}]}]`)
var settleABI = string(evm.X402ExactPermit2ProxySettleABI)

const approveABI = `[{"name":"approve","type":"function","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"type":"bool"}]}]`
const batchABI = `[{"name":"executeBatch","type":"function","inputs":[{"name":"calls","type":"tuple[]","components":[{"name":"target","type":"address"},{"name":"value","type":"uint256"},{"name":"data","type":"bytes"}]}],"outputs":[]}]`

var decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var hexPattern = regexp.MustCompile(`^0x(0|[1-9a-f][0-9a-f]*)$`)
var chainPattern = regexp.MustCompile(`^eip155:[1-9][0-9]*$`)

type expectedPayment struct {
	asset            common.Address
	amount, deadline *big.Int
	chainID          uint64
	callData         []byte
}
type tokenPermission struct {
	Token  common.Address
	Amount *big.Int
}
type permit struct {
	Permitted       tokenPermission
	Nonce, Deadline *big.Int
}
type witness struct {
	To         common.Address
	ValidAfter *big.Int
}
type call struct {
	Target common.Address
	Value  *big.Int
	Data   []byte
}

func paymentBatch(payment x402.PaymentPayload, owner common.Address) (expectedPayment, error) {
	r := payment.Accepted
	var zero expectedPayment
	if payment.X402Version != 2 || !chainPattern.MatchString(r.Network) {
		return zero, invalid("payment network")
	}
	chain, err := strconv.ParseUint(strings.TrimPrefix(r.Network, "eip155:"), 10, 53)
	if err != nil {
		return zero, invalid("chain identifier")
	}
	data, err := json.Marshal(payment.Payload)
	if err != nil {
		return zero, invalid("payment payload")
	}
	var p struct {
		Signature     string `json:"signature"`
		Authorization struct {
			Permitted struct {
				Token  string `json:"token"`
				Amount string `json:"amount"`
			} `json:"permitted"`
			From     string `json:"from"`
			Spender  string `json:"spender"`
			Nonce    string `json:"nonce"`
			Deadline string `json:"deadline"`
			Witness  struct {
				To         string `json:"to"`
				ValidAfter string `json:"validAfter"`
			} `json:"witness"`
		} `json:"permit2Authorization"`
	}
	if json.Unmarshal(data, &p) != nil {
		return zero, invalid("Permit2 authorization")
	}
	a := p.Authorization
	amount, ok := decimal(a.Permitted.Amount)
	if !ok || amount.Sign() == 0 || a.Permitted.Amount != r.Amount {
		return zero, invalid("payment amount")
	}
	deadline, ok := decimal(a.Deadline)
	if !ok || deadline.Cmp(big.NewInt(time.Now().Unix())) <= 0 {
		return zero, invalid("payment deadline")
	}
	nonce, ok := decimal(a.Nonce)
	if !ok {
		return zero, invalid("payment nonce")
	}
	validAfter, ok := decimal(a.Witness.ValidAfter)
	if !ok {
		return zero, invalid("payment validAfter")
	}
	if !sameAddress(a.From, owner.Hex()) || !sameAddress(a.Permitted.Token, r.Asset) || !sameAddress(a.Spender, x402.Permit2Proxy) || !sameAddress(a.Witness.To, r.PayTo) {
		return zero, invalid("payment authorization")
	}
	proxy, ok := r.Extra["permit2Proxy"].(string)
	if !ok || !sameAddress(proxy, x402.Permit2Proxy) {
		return zero, invalid("Permit2 proxy")
	}
	signature, ok := hexBytes(p.Signature)
	if !ok || len(signature) != 65 {
		return zero, invalid("payment signature")
	}
	asset := common.HexToAddress(r.Asset)
	settle := mustEncodeCall(settleABI, "settle", permit{tokenPermission{asset, amount}, nonce, deadline}, owner, witness{common.HexToAddress(r.PayTo), validAfter}, signature)
	approve := mustEncodeCall(approveABI, "approve", common.HexToAddress(x402.Permit2), amount)
	batch := mustEncodeCall(batchABI, "executeBatch", []call{{asset, new(big.Int), approve}, {common.HexToAddress(x402.Permit2Proxy), new(big.Int), settle}})
	return expectedPayment{asset, amount, deadline, chain, batch}, nil
}

// Callers supply fixed contract ABIs and validated, concrete argument types.
func mustEncodeCall(definition, name string, args ...any) []byte {
	parsed, err := abi.JSON(strings.NewReader(definition))
	if err != nil {
		panic(err)
	}
	data, err := parsed.Pack(name, args...)
	if err != nil {
		panic(err)
	}
	return data
}
func decimal(s string) (*big.Int, bool) {
	if len(s) > 78 || !decimalPattern.MatchString(s) {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	return n, ok && n.BitLen() <= 256
}
func quantity(s string, bits int) (*big.Int, bool) {
	if len(s) > 2+(bits+3)/4 || !hexPattern.MatchString(s) {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s[2:], 16)
	return n, ok && n.BitLen() <= bits
}
func hexBytes(s string) ([]byte, bool) {
	if !strings.HasPrefix(s, "0x") {
		return nil, false
	}
	b, err := hex.DecodeString(s[2:])
	return b, err == nil
}
