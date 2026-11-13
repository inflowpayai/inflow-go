package seller

import (
	"context"
	"errors"
	"net/http"
	"strings"

	inflow "github.com/inflowpayai/inflow-go"
	"github.com/inflowpayai/inflow-go/internal/requestpricing"
	foundation "github.com/x402-foundation/x402/go/v2"
	xhttp "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

// RequestPricingServer configures an isolated upstream server for each request.
// Options and Configure must be concurrency-safe. Shared facilitator clients retain
// their configuration cache; Configure installs HTTP and payment lifecycle hooks.
type RequestPricingServer struct {
	// RoutePattern uses upstream syntax, such as "GET /widgets/:id".
	// Empty matches every request. Bazaar discovery requires an explicit pattern.
	RoutePattern string
	Options      []foundation.ResourceServerOption
	Configure    func(*xhttp.HTTPServer)
}

// ProtectRequest resolves a complete route once per request and delegates payment
// processing to upstream net/http middleware. Mount it only on protected routes.
// Register schemes through server configuration. The resolver
// must be concurrency-safe and honor the request context. Body bytes are opt-in;
// the resolver's request has no readable body.
func ProtectRequest(server RequestPricingServer, options inflow.RequestPricingOptions, resolve func(*http.Request, []byte) (xhttp.RouteConfig, error), next http.Handler, middlewareOptions ...nethttp.MiddlewareOption) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("request pricing requires a handler")
	}
	serverOptions := append([]foundation.ResourceServerOption(nil), server.Options...)
	optionsCopy := append([]nethttp.MiddlewareOption(nil), middlewareOptions...)
	optionsCopy = append(optionsCopy, nethttp.WithSyncFacilitatorOnStart(false))
	pattern := strings.TrimSpace(server.RoutePattern)
	return requestpricing.Handler(options, resolve, func(ctx context.Context, route xhttp.RouteConfig, _ []byte) (http.Handler, error) {
		if len(route.Accepts) == 0 {
			return nil, errors.New("request pricing requires at least one payment offer")
		}
		routePattern := pattern
		if routePattern == "" {
			if _, advertised := route.Extensions["bazaar"]; advertised {
				return nil, errors.New("Bazaar discovery requires a route pattern")
			}
			routePattern = "*"
		}
		resource := xhttp.Newx402HTTPResourceServer(xhttp.RoutesConfig{routePattern: route}, serverOptions...)
		if server.Configure != nil {
			server.Configure(resource)
		}
		if err := resource.Initialize(ctx); err != nil {
			return nil, err
		}
		protected := nethttp.PaymentMiddlewareFromHTTPServer(resource, optionsCopy...)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Upstream passes unmatched requests through; this handler is exclusively protected.
			if !resource.RequiresPayment(xhttp.HTTPRequestContext{Path: r.URL.EscapedPath(), DecodedPath: r.URL.Path, Method: r.Method}) {
				w.Header().Set("Cache-Control", "no-store")
				http.NotFound(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		}), nil
	})
}
