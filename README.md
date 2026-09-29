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

### x402 Core

Import `github.com/inflowpayai/inflow-go/x402` for V2 payment types, InFlow configuration types,
payment identifiers, and sponsorship declarations. Standard payment types are aliases of
`github.com/x402-foundation/x402/go/v2` v2.27.0 types: pass them to upstream APIs without conversions.
Importing this package does not import blockchain signers or framework adapters.

```go
id, err := x402.GeneratePaymentID(x402.DefaultPaymentIDPrefix)
if err != nil {
    return err
}
entry := x402.PaymentIdentifierEntry(x402.DeclarePaymentIdentifier(), id)
payload := x402.PaymentPayload{
    X402Version: x402.Version,
    Extensions: map[string]any{x402.PaymentIdentifier: entry},
}
```

This illustrates the extension field, not a complete signed payment. The declaration advertises
`required: false`. `ReadPaymentIdentifier` and `PaymentIdentifierEntry` return nil for malformed
declarations or invalid identifiers. Entries preserve additional information and schema fields,
without modifying the supplied declaration. Use an empty prefix to generate an unprefixed identifier.

`NormalizeDecimalString` removes insignificant zeroes using string operations; it does not round
amounts or convert them into atomic units. Non-plain notation such as `1e3` is returned unchanged.
The upstream payload and extension maps can contain `json.Number`; when decoding JSON with large
numeric proof values, use `json.Decoder.UseNumber` to avoid conversion to floating-point numbers.
These types and declarations do not validate signatures, authorize payments, or execute sponsorship.

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

### Managed buyer credentials

Use `mpp/buyer` to obtain a credential for a seller's challenge. The client supports InFlow
charge and subscription challenges and Tempo charge challenges. `Fulfil` and `Prepare` send
payment requests to InFlow; they do not sign locally or send the credential to the seller's resource.

```go
client, err := buyer.New(buyer.Options{
    Options: inflow.Options{
        Environment: inflow.Sandbox,
        APIKey: os.Getenv("INFLOW_API_KEY"),
    },
})
if err != nil {
    return err
}
credential, err := client.Fulfil(ctx, challenge, buyer.PaymentOptions{})
```

Import `github.com/inflowpayai/inflow-go/mpp/buyer` alongside the root `inflow` package.
The challenge comes from `mpp.ParseChallenges`. Keep its encoded request and opaque fields intact.
For an InFlow instrument charge, supply `PaymentOptions.InstrumentID`. For access under an existing
InFlow subscription, supply `PaymentOptions.SubscriptionID`; that calls subscription authorization
instead of creating another purchase. Tempo requires no per-call selector.

For a separate creation and waiting step, call `Prepare`, then `payment.Wait(ctx)` or
`payment.Cancel(ctx)`. `TransactionID()` and `ApprovalID()` expose the initial response identifiers.
Concurrent waits share one polling sequence and result; each successful wait receives its own
decoded credential. Cancelling a wait abandons that payment for every waiter, not other payments
using the same client. A completed result remains available on the handle.

`PollInterval` defaults to five seconds; the server's `retryAfterSeconds` takes precedence, including
zero. `WaitTimeout` defaults to fifteen minutes after creation returns. It includes time before
`Wait` and time spent making polling requests. The context passed to `Prepare` owns the operation;
keep it alive until waiting or cancellation finishes.

When an unfinished payment fails or is cancelled, the SDK attempts cancellation of its known
approval and waits up to five seconds for that attempt. This cleanup uses a separate context so an
already-cancelled payment context does not prevent it. Cleanup failure never replaces the payment
error. Explicit `Cancel` returns the cleanup error, or its own caller context error if that caller
stops waiting. No approval can be cancelled when creation ends without receiving its identifier.
Cancelling subscription authorization does not cancel the subscription, and cancelling a completed
payment does not reverse it.

Use `errors.As` with `*buyer.Error` for payment failures. Its `Code` distinguishes cancellation,
pending timeout, platform rejection, expiry, malformed responses or credentials, and unsupported
methods. `Problem` retains the platform's problem JSON. Cancellation and timeout support
`errors.Is` with the corresponding context error. HTTP failures retain `*inflow.APIError`.
Creation, authorization, polling, and cleanup requests make one attempt each; the SDK does not
replay a payment workflow after an uncertain network outcome.

### Payment-aware HTTP requests

`client.Do(request, paymentOptions)` sends a resource request, handles a `402` by fulfilling the
first supported MPP challenge in the server's order, and sends one paid retry. Use `Fulfil` or
`Prepare` when your application needs to choose a particular challenge itself. Close the returned
response body, just as with `http.Client.Do`. InFlow API authentication is not sent to the resource.

```go
request, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
if err != nil {
    return err
}
request.Header.Set("X-AEP-API-Key", serviceAPIKey)
response, err := client.Do(request, buyer.PaymentOptions{})
if err != nil {
    return err
}
defer response.Body.Close()
```

MPP sends its credential in `Authorization: Payment ...`. If a `402` request already has a
nonempty `Authorization` header, or URL credentials that produce Basic authentication, `Do`
returns `ErrAuthorizationConflict` before obtaining a payment credential. Separate API-key and
cookie headers are preserved. Ordinary non-402 responses, including authenticated ones, pass
through without a payment attempt. The SDK does not move application credentials to another header.

A request with a body must supply a working `GetBody` before payment starts. `http.NewRequest`
provides it for `strings.Reader`, `bytes.Reader`, and `bytes.Buffer` inputs. The SDK does not buffer
an arbitrary streaming body. It closes an intermediate `402` body without draining it and does
not modify the caller's headers or `GetBody`. The resource transport is `Options.Transport`;
`Options.Timeout` applies to each resource exchange, including response-body reading.

Neither the initial nor the paid request follows redirects. A paid response of `401`, `402`,
or another failure status is returned as-is; it does not initiate another payment. A network
failure after submitting a credential can leave the payment outcome unknown. The SDK does not
retry that request or attempt to reverse payment. Inspect the result before retrying in your
application. `Payment-Receipt`, when supplied by the seller, remains on the response for decoding
with `mpp.DecodeReceipt`.

### Accepting payments

Import `github.com/inflowpayai/inflow-go/mpp/seller` and create a client with an API key from an
InFlow **Seller** account: [Sandbox](https://sandbox.inflowpay.ai) for testing or
[Production](https://app.inflowpay.ai) for live payments. A Developer key does not authorize
Seller configuration, validation, or broadcast.

```go
client, err := seller.New(inflow.Options{
    Environment: inflow.Sandbox,
    APIKey: os.Getenv("INFLOW_API_KEY"),
})
if err != nil {
    return err
}
handler, err := client.Protect(seller.Route{
    Realm: "api.example.com",
    SecretKey: os.Getenv("MPP_SECRET_KEY"),
    Offers: []seller.Offer{{Charge: &mpp.ChargeRequest{
        Amount: "0.01",
        Currency: "USDC",
    }}},
}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.Write([]byte("paid response"))
}))
if err != nil {
    return err
}
mux.Handle("/paid", handler)
```

Use a private, high-entropy `MPP_SECRET_KEY`, separate from the InFlow API key. Keep the same
signing key across instances serving the same routes. Changing it invalidates outstanding
challenges. `Realm` identifies the protected service; `Lifetime` defaults to five minutes.

An `Offer` selects exactly one of `Charge`, `Subscription`, or `Tempo`. Use multiple offers to
advertise alternative prices or payment methods. InFlow offers obtain their recipient from the
Seller account and select an advertised currency/rail combination. If multiple rails are available,
set `MethodDetails.Rail`; required instrument identifiers must also be provided. Subscription
requests include their recurring terms. Tempo requests specify the token address and recipient;
their method details default to `feePayer: false` and `supportedModes: ["pull"]`.

`Protect` returns an ordinary `http.Handler`. Without a valid payment it returns a `402` challenge.
It verifies the echoed challenge signature, expiry, realm, opaque data, and configured offer before
requesting platform validation or broadcast. It then sets `Payment-Receipt` and runs the
application handler. Payment happens **before** the handler: a handler failure does not reverse it.
The response is not buffered, so the handler can stream normally after payment succeeds.

Place application authentication outside this handler. MPP uses `Authorization: Payment ...`;
separate API-key or cookie authentication avoids conflicting with that header. `CanOffer` filters
which configured offers are advertised. It is not access control and does not revoke credentials
already issued for a matching offer. Optional `Opaque` is an encoded value signed into the
challenge; use distinct values when otherwise identical offers must not be interchangeable across
routes. The middleware does not bind the HTTP request body to payment; enforce application-specific
request authorization in your own handler or middleware.

For custom integrations, `Prepare` resolves a typed offer without minting a challenge. `Validate`
performs the non-consuming platform check, `Broadcast` performs the terminal payment operation,
and `Verify` validates then broadcasts. These direct methods do not establish that a credential
belongs to your HTTP route: that local signature and route check belongs to `Protect`, or to your
own protocol integration. Inspect `*seller.Error` for capability failures or rejected payment;
`Problem` preserves the platform's problem JSON. Transport and account-role failures remain
`*inflow.APIError`.

Configuration loads on demand. `Load(ctx)` performs an explicit startup check; concurrent callers
share the active load and successful results remain cached. A failed load permits a later attempt.
The caller initiating a shared load owns its request context; cancellation can fail that shared
attempt, while another call can retry. Cancelling a waiting caller does not cancel the owner's load.

Configuration and validation allow up to three transient retries. Broadcast retries are enabled
only when configuration advertises idempotency keys, and reuse one key throughout that operation.
Pass a stable key to `Broadcast` when explicitly retrying an uncertain outcome; an empty key
generates a fresh one. Without advertised idempotency support, broadcast makes one attempt.
Never retry the entire protected application request solely because its response was lost.

### Upstream compatibility notes

These observations apply to `mpp-go v0.2.0` ([source revision](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251)). They distinguish the library's general-purpose behavior from the requirements of the [InFlow MPP integration](https://github.com/inflowpayai/inflow-specs/blob/main/contracts/mpp.md). Recheck them when upgrading the dependency.

1. **Preserving application authentication.** The upstream buyer transport puts the payment credential in `Authorization`, replacing an existing value. An authenticated resource may already use that header for its application session. InFlow's HTTP integration rejects that conflict before payment and preserves separate cookie/API-key authentication. It does not invent an alternate payment header. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L99-L106).

2. **Checking request replay before payment.** The upstream transport creates a payment credential before checking whether the request body can be replayed. For a body without `GetBody`, this can invoke the payment method and then fail locally without sending the paid request. InFlow must establish replayability before invoking a payment method. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L92-L125).

3. **Separate validation and broadcast.** The upstream seller `Intent` interface exposes one `Verify` operation returning a receipt. InFlow exposes non-mutating validation separately from the terminal broadcast operation. Its seller integration must retain both operations rather than hide broadcasting inside an API presented as validation. The upstream interface can represent a combined operation, but not both phases independently. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L17-L27).

4. **Subscription entry points.** The upstream generic verification function accepts an arbitrary intent, but the `Charge` helper selects `charge` explicitly, and `ComposeMiddleware` operates on charge configurations. InFlow subscriptions therefore need their own orchestration; this is a limitation of those convenience APIs, not an inability to encode subscription challenges. [Charge source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L148-L164), [composition source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/compose.go#L12-L37).

5. **Preserving receipt fields.** The upstream receipt type has a fixed set of fields and a nested `extra` object. InFlow receipts also carry top-level fields such as `challengeId`, `subscriptionId`, and `settlement`. The upstream receipt parser/formatter does not preserve those top-level fields; moving them into `extra` changes the wire format. InFlow needs a receipt representation and codec that retain its contract. [Type](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/receipt.go#L7-L14), [parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L502-L537).

6. **Server dependency coupling.** The upstream server package imports its Tempo package, which brings Ethereum, Tempo, and Redis packages into the compilation dependencies. InFlow delegates payment processing to its platform and does not need that entire server implementation for this purpose. Using the protocol primitives avoids this coupling. The upstream module's web-framework requirements do not mean every framework is compiled into every consumer. This distinction was checked with `go list -deps`. [Server imports](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L9-L15), [Tempo Redis dependency](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/tempo/redis_store.go#L3-L8).

7. **MPP over MCP is not supported.** The released Go library has no MCP integration package corresponding to the `mppx/mcp/client` integration used by InFlow Node. InFlow Go does not implement an independent MPP-over-MCP transport. Support depends on an upstream implementation so that integrators do not adopt an InFlow-specific design that could conflict with the upstream protocol integration. This limitation concerns MPP, not x402's separate MCP integration. [Released package tree](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg).

8. **Preserving opaque challenge data.** The upstream challenge parser decodes `opaque` into a string map, and its formatter encodes that map again. An issuer's original encoded value can therefore change. InFlow keeps that field as its original string and uses its own wire types, while reusing compatible upstream header primitives. [Parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L213-L298).
