package inflow

import (
	"context"
	"errors"
	"testing"
)

func TestAPIError(t *testing.T) {
	err := &APIError{Code: "NETWORK_ERROR", Message: "InFlow request cancelled", Cause: context.Canceled}
	if err.Error() != "InFlow request cancelled" || !errors.Is(err, context.Canceled) {
		t.Fatal("error message or cancellation cause was lost")
	}
	if (&APIError{}).Unwrap() != nil {
		t.Fatal("unexpected cause")
	}
}
