# InFlow Go SDK

Go SDK for accepting and making InFlow payments through MPP and x402.

## Development

Requires Go 1.26 or later and Make. Run `make verify` for formatting, module tidiness, compilation,
static analysis, race-enabled tests, package documentation, and a build from a separate consumer
module. `make format` formats Go source files.

CI runs on Go 1.26 and 1.27. Local tests require at least 99% statement coverage in every source
file. Codecov evaluates 99% project and changed-line targets without tolerance. Its line-based
measurements differ from Go's statement coverage; aim for 100% on both.

The consumer check builds and runs a separate Go module against the local checkout. It checks
configuration and error types independently of payment behavior.

## Package design

One Go module carries one release version. MPP and x402 each have Core, Buyer, and Seller
packages, with shared internal HTTP implementation. The client accepts an optional
`http.RoundTripper`; InFlow controls HTTP redirect and timeout policies. Construction performs no
network activity. Operations load configuration when needed and permit a later attempt after a
failed load.

### Shared configuration and errors

The protocol clients accept `inflow.Options`. Set `Environment: inflow.Sandbox` for testing;
the default is production. `BaseURL` overrides the environment address for a private deployment
or local testing. Configure either `APIKey` or an `AccessToken` callback, not both. Omit both for
anonymous requests to endpoints that permit them. The callback receives the request context,
runs for each attempt, and must support concurrent calls and cancellation. Its errors return
unchanged to the caller.

`Timeout` defaults to 30 seconds per attempt, including token retrieval and response-body reading.
An earlier deadline on the operation's context takes precedence. Custom transports must honor
that context and must not follow redirects; the SDK does not forward requests to redirect targets.
API request and response bodies are limited to 8 MiB.

Use `errors.As` to inspect `*inflow.APIError` for the server's error code, message, HTTP status,
request identifier, and diagnostic response. Credential headers and recognized credential fields
are redacted from error diagnostics. Transport failures have HTTP status zero; cancellation and
deadlines support `errors.Is`. Error diagnostics can still contain application data and are not
a substitute for an application's logging policy.

HTTP request methods are internal. Protocol operations choose their retry policy explicitly;
the shared transport performs no retries by default. Cancelling a request context stops local
work, but does not itself cancel a server-side approval or reverse a payment.

## MPP integration design

The integration uses `github.com/tempoxyz/mpp-go` protocol primitives, with InFlow-owned HTTP handling and payment lifecycle orchestration. It does not fork the upstream library.

### Payment data and codecs

Import `github.com/inflowpayai/inflow-go/mpp` to read payment challenges, credentials, and receipts.
For a protected resource's response, pass `response.Header.Values("WWW-Authenticate")` to
`mpp.ParseChallenges`. It accepts repeated and combined headers, preserves challenge order, and
ignores unrelated authentication schemes such as Bearer. A malformed Payment challenge returns
`*mpp.CodecError`; it is not silently removed from the result.

`Challenge.Request` and `Challenge.Opaque` retain the issuer's encoded strings. Keep those values
unchanged when echoing a challenge in a credential: decoding and re-encoding them can change the
seller's challenge binding. Optional challenge fields use pointers so omission and an explicitly
empty value remain distinct.

```go
request := mpp.ChargeRequest{
    Amount: "10.5",
    Currency: "USDC",
    MethodDetails: &mpp.InflowMethodDetails{Rail: "balance"},
}
if err := request.Validate(); err != nil {
    return err
}
encodedRequest, err := mpp.Encode(request)
```

`ChargeRequest`, `SubscriptionRequest`, `TempoRequest`, and `TempoPayload` provide native shape
validation. This does not establish that an account supports a currency or rail, or that a proof
is valid. Amounts are strings: InFlow amounts use decimal units; Tempo amounts use integer base
units. Request encoding sorts keys and omits null object members, matching InFlow's request
encoding. Use strings for exact monetary values; general numeric inputs use binary64 semantics.

`DecodeCredential` and `EncodeCredential` preserve payload values, including nulls and large
integers decoded as `json.Number`. `DecodeReceipt` and `EncodeReceipt` preserve InFlow settlement
fields and arbitrary top-level method extensions in `Receipt.Extensions`. Extensions cannot
overwrite the receipt's named fields. These functions take or return the base64url value alone,
without a `Payment ` prefix. Decoding checks structure, not payment validity or settlement.

Codecs accept at most 64 KiB of encoded data per value or header. Errors identify the invalid
artifact without copying credential contents into the message. The MPP package compiles only the
upstream protocol-primitives package and the Go standard library; it does not import the upstream
server, blockchain clients, or Redis integration.

### Upstream compatibility notes

These observations apply to `mpp-go v0.2.0` ([source revision](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251)). They distinguish the library's general-purpose behavior from the requirements of the [InFlow MPP integration](https://github.com/inflowpayai/inflow-specs/blob/main/contracts/mpp.md). Recheck them when upgrading the dependency.

1. **Preserving application authentication.** The upstream buyer transport puts the payment credential in `Authorization`, replacing an existing value. An authenticated resource may already use that header for its application session. InFlow's HTTP integration must preserve application authentication and use the appropriate payment header instead of delegating this retry unchanged. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L99-L106).

2. **Checking request replay before payment.** The upstream transport creates a payment credential before checking whether the request body can be replayed. For a body without `GetBody`, this can invoke the payment method and then fail locally without sending the paid request. InFlow must establish replayability before invoking a payment method. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L92-L125).

3. **Separate validation and broadcast.** The upstream seller `Intent` interface exposes one `Verify` operation returning a receipt. InFlow exposes non-mutating validation separately from the terminal broadcast operation. Its seller integration must retain both operations rather than hide broadcasting inside an API presented as validation. The upstream interface can represent a combined operation, but not both phases independently. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L17-L27).

4. **Subscription entry points.** The upstream generic verification function accepts an arbitrary intent, but the `Charge` helper selects `charge` explicitly, and `ComposeMiddleware` operates on charge configurations. InFlow subscriptions therefore need their own orchestration; this is a limitation of those convenience APIs, not an inability to encode subscription challenges. [Charge source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L148-L164), [composition source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/compose.go#L12-L37).

5. **Preserving receipt fields.** The upstream receipt type has a fixed set of fields and a nested `extra` object. InFlow receipts also carry top-level fields such as `challengeId`, `subscriptionId`, and `settlement`. The upstream receipt parser/formatter does not preserve those top-level fields; moving them into `extra` changes the wire format. InFlow needs a receipt representation and codec that retain its contract. [Type](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/receipt.go#L7-L14), [parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L502-L537).

6. **Server dependency coupling.** The upstream server package imports its Tempo package, which brings Ethereum, Tempo, and Redis packages into the compilation dependencies. InFlow delegates payment processing to its platform and does not need that entire server implementation for this purpose. Using the protocol primitives avoids this coupling. The upstream module's web-framework requirements do not mean every framework is compiled into every consumer. This distinction was checked with `go list -deps`. [Server imports](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L9-L15), [Tempo Redis dependency](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/tempo/redis_store.go#L3-L8).

7. **MPP over MCP is not supported.** The released Go library has no MCP integration package corresponding to the `mppx/mcp/client` integration used by InFlow Node. InFlow Go does not implement an independent MPP-over-MCP transport. Support depends on an upstream implementation so that integrators do not adopt an InFlow-specific design that could conflict with the upstream protocol integration. This limitation concerns MPP, not x402's separate MCP integration. [Released package tree](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg).

8. **Preserving opaque challenge data.** The upstream challenge parser decodes `opaque` into a string map, and its formatter encodes that map again. An issuer's original encoded value can therefore change. InFlow keeps that field as its original string and uses its own wire types, while reusing compatible upstream header primitives. [Parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L213-L298).
