package buyer

import (
	"testing"

	"github.com/inflowpayai/inflow-go/mpp"
)

func TestLimitsRejectUndecodableRequest(t *testing.T) {
	check, err := newMethodLimits(mpp.MethodInflow, mpp.IntentCharge, []MethodLimits{{MaxAmount: "1", Currencies: []string{"USDC"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{"not base64", "bm90LWpzb24"} {
		if err := check(mpp.Challenge{Request: request}); err == nil {
			t.Fatal("limits accepted an undecodable payment request")
		}
	}
}
