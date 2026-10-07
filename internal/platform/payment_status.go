package platform

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	inflow "github.com/inflowpayai/inflow-go"
)

func (c *Client) PaymentStatus(ctx context.Context, transactionID string, options inflow.PaymentStatusOptions) (inflow.PaymentStatus, error) {
	segment := strings.ReplaceAll(url.QueryEscape(transactionID), "+", "%20")
	raw, err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/transactions/" + segment, Retries: options.Retries})
	if err != nil {
		return inflow.PaymentStatus{}, err
	}
	return Decode[inflow.PaymentStatus](raw)
}
