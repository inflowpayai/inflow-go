# InFlow Go SDK

Go SDK for accepting and making InFlow payments through MPP and x402.

## Development

Requires Go 1.26 or later and Make. Run `make verify` for formatting, module tidiness, compilation,
static analysis, race-enabled tests, package documentation, and a build from a separate consumer
module. `make format` formats Go source files.

CI runs on Go 1.26 and 1.27. Codecov evaluates an 85% project target with a 1% tolerance and a
90% changed-line target without tolerance.

The consumer check builds a separate Go module against the local checkout. It checks module
consumption independently of payment behavior.

## Package design

One Go module carries one release version. MPP and x402 each have Core, Buyer, and Seller
packages, with shared internal HTTP implementation. The client accepts an optional
`http.RoundTripper`; InFlow controls HTTP redirect and timeout policies. Construction performs no
network activity. Operations load configuration when needed and permit a later attempt after a
failed load.

## MPP integration design

The integration uses `github.com/tempoxyz/mpp-go` protocol primitives, with InFlow-owned HTTP handling and payment lifecycle orchestration. It does not fork the upstream library.

### Upstream compatibility notes

These observations apply to `mpp-go v0.2.0` ([source revision](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251)). They distinguish the library's general-purpose behavior from the requirements of the [InFlow MPP integration](https://github.com/inflowpayai/inflow-specs/blob/main/contracts/mpp.md). Recheck them when upgrading the dependency.

1. **Preserving application authentication.** The upstream buyer transport puts the payment credential in `Authorization`, replacing an existing value. An authenticated resource may already use that header for its application session. InFlow's HTTP integration must preserve application authentication and use the appropriate payment header instead of delegating this retry unchanged. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L99-L106).

2. **Checking request replay before payment.** The upstream transport creates a payment credential before checking whether the request body can be replayed. For a body without `GetBody`, this can invoke the payment method and then fail locally without sending the paid request. InFlow must establish replayability before invoking a payment method. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L92-L125).

3. **Separate validation and broadcast.** The upstream seller `Intent` interface exposes one `Verify` operation returning a receipt. InFlow exposes non-mutating validation separately from the terminal broadcast operation. Its seller integration must retain both operations rather than hide broadcasting inside an API presented as validation. The upstream interface can represent a combined operation, but not both phases independently. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L17-L27).

4. **Subscription entry points.** The upstream generic verification function accepts an arbitrary intent, but the `Charge` helper selects `charge` explicitly, and `ComposeMiddleware` operates on charge configurations. InFlow subscriptions therefore need their own orchestration; this is a limitation of those convenience APIs, not an inability to encode subscription challenges. [Charge source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L148-L164), [composition source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/compose.go#L12-L37).

5. **Preserving receipt fields.** The upstream receipt type has a fixed set of fields and a nested `extra` object. InFlow receipts also carry top-level fields such as `challengeId`, `subscriptionId`, and `settlement`. The upstream receipt parser/formatter does not preserve those top-level fields; moving them into `extra` changes the wire format. InFlow needs a receipt representation and codec that retain its contract. [Type](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/receipt.go#L7-L14), [parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L502-L537).

6. **Server dependency coupling.** The upstream server package imports its Tempo package, which brings Ethereum, Tempo, and Redis packages into the compilation dependencies. InFlow delegates payment processing to its platform and does not need that entire server implementation for this purpose. Using the protocol primitives avoids this coupling. The upstream module's web-framework requirements do not mean every framework is compiled into every consumer. This distinction was checked with `go list -deps`. [Server imports](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L9-L15), [Tempo Redis dependency](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/tempo/redis_store.go#L3-L8).

7. **MPP over MCP is not supported.** The released Go library has no MCP integration package corresponding to the `mppx/mcp/client` integration used by InFlow Node. InFlow Go does not implement an independent MPP-over-MCP transport. Support depends on an upstream implementation so that integrators do not adopt an InFlow-specific design that could conflict with the upstream protocol integration. This limitation concerns MPP, not x402's separate MCP integration. [Released package tree](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg).
