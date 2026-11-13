package requestpricing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"

	inflow "github.com/inflowpayai/inflow-go"
)

// Handler isolates callback input and preserves the application request body.
func Handler[T any](options inflow.RequestPricingOptions, resolve func(*http.Request, []byte) (T, error), build func(context.Context, T, []byte) (http.Handler, error)) (http.Handler, error) {
	if resolve == nil || options.MaxBodyBytes < 0 || options.MaxBodyBytes == math.MaxInt64 {
		return nil, errors.New("request pricing requires a resolver and a valid body limit")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, err error) {
			if options.OnError != nil {
				options.OnError(r, err)
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, http.StatusText(status), status)
		}
		if err := r.Context().Err(); err != nil {
			fail(http.StatusInternalServerError, err)
			return
		}
		var body []byte
		if options.MaxBodyBytes > 0 && r.Body != nil {
			data, err := io.ReadAll(io.LimitReader(r.Body, options.MaxBodyBytes+1))
			_ = r.Body.Close()
			if err != nil {
				fail(http.StatusBadRequest, err)
				return
			}
			if int64(len(data)) > options.MaxBodyBytes {
				fail(http.StatusRequestEntityTooLarge, errors.New("request exceeds pricing body limit"))
				return
			}
			body = data
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		input := r.Clone(r.Context())
		input.Body = http.NoBody
		input.GetBody = nil
		value, err := resolve(input, bytes.Clone(body))
		if err == nil {
			err = r.Context().Err()
		}
		if err != nil {
			fail(http.StatusInternalServerError, err)
			return
		}
		handler, err := build(r.Context(), value, body)
		if err != nil {
			fail(http.StatusInternalServerError, err)
			return
		}
		handler.ServeHTTP(w, r)
	}), nil
}
