package inflow

// PaymentStatus describes settlement, not approval or payment-credential readiness.
type PaymentStatus struct {
	TransactionID string         `json:"transactionId"`
	Status        string         `json:"status"`
	NextAction    *PaymentAction `json:"nextAction,omitempty"`
}

// PaymentAction describes a buyer step such as authenticate_card. The URL is an
// authenticated dashboard page; do not send API credentials to it.
type PaymentAction struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type PaymentStatusOptions struct {
	// Retries defaults to zero (one attempt) and is capped at three.
	Retries int
}
