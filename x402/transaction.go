package x402

import "regexp"

const InflowTransaction = "inflow-transaction"

var transactionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// InflowTransactionID returns the seller's InFlow transaction ID from a successful
// settlement, or an empty string when the optional identifier is absent or malformed.
func InflowTransactionID(result *SettleResponse) string {
	if result == nil || !result.Success {
		return ""
	}
	info := object(object(result.Extensions[InflowTransaction])["info"])
	id, _ := info["transactionId"].(string)
	if len(id) != 36 || !transactionIDPattern.MatchString(id) {
		return ""
	}
	return id
}
