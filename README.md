# InFlow Go SDK

Go SDK for accepting and making InFlow payments through MPP and x402.

For development checks and reproducible SDK conformance reports, see
[Shared conformance](conformance/README.md).

## Run an example

The [runnable examples](examples/README.md) walk through MPP and x402 payments with separate
buyer and seller programs. They use Sandbox accounts, local HTTP sellers, and the public clients.
Start there for setup commands, required credentials, expected output, subscriptions, and manual
approval waiting or cancellation.

## Using InFlow with the upstream Go SDKs

InFlow Go provides clients for making and accepting payments through InFlow. It uses the
[MPP Go SDK](https://github.com/tempoxyz/mpp-go) and the
[x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go), but it is not a drop-in
replacement for either library. Adopt the InFlow clients explicitly; changing an import does
not preserve every upstream behavior. You can keep using upstream packages alongside InFlow.

In this guide, **managed payments** are handled through your InFlow account. **External-wallet
payments** are signed by a wallet configured in your application. A **payment credential** or
**payment payload** is the proof your application sends to the seller; obtaining it does not
by itself confirm that the seller accepted the payment. A **hook** is an application callback:
an after-hook runs after a payment payload has been created.

### Adopting the MPP clients

The `mpp/buyer` client asks InFlow to obtain a payment credential rather than signing through
a local wallet. Configure your InFlow account credentials and environment, then use `Fulfil`
to obtain a credential or `Do` to pay an HTTP resource.
The `mpp/seller` client uses an InFlow Seller account to validate and process payments.
See [Managed buyer credentials](#managed-buyer-credentials) or
[Accepting payments](#accepting-payments) for setup and examples.

The following are intentional InFlow behaviors. The comparison applies to `mpp-go v0.2.0`;
[upstream compatibility notes](#upstream-compatibility-notes) link to the relevant source.

| Situation | Upstream MPP Go | InFlow Go | What your application needs to do |
| --- | --- | --- | --- |
| A paid request already uses `Authorization` | The buyer transport replaces it with the payment credential. | `Do` rejects the conflict before obtaining a credential. | Use a separate API-key header or cookie supported by the service. If the service only accepts application authentication in `Authorization`, its authentication design must accommodate MPP before this client can pay it. Manually sending the request does not resolve that conflict. |
| A request body cannot be replayed | The transport creates a credential before trying to recreate the body. | `Do` checks replayability before payment. | Supply a working `Request.GetBody` for a request with a body. |
| A seller checks a payment | The seller intent interface provides one `Verify` operation. | `Validate` checks without consuming payment; `Broadcast` submits the credential for payment processing. `Verify` combines both. | Use `Validate` only for a check. Use `Verify` or `Protect` when payment must be processed before serving the resource. |
| Your service offers subscriptions | The charge convenience helpers select the charge intent. | InFlow supports purchasing a subscription and authorizing access under an existing subscription. | Sellers use `seller.Offer.Subscription`. Buyers pass a subscription challenge to `Fulfil`; supply `PaymentOptions.SubscriptionID` only to use an existing subscription rather than purchase one. See [Accepting payments](#accepting-payments) and [Managed buyer credentials](#managed-buyer-credentials). |
| Your application forwards challenges or receipts | Upstream types decode opaque challenge data and do not retain all InFlow top-level receipt fields. | InFlow types preserve the encoded challenge fields and top-level receipt extensions. | Use the InFlow MPP codecs throughout that exchange; decoding and rebuilding through upstream types can lose information. |

**Upstream limitation:** MPP over MCP is not supported by the released `mpp-go v0.2.0`
integration or by InFlow Go. Do not assume the MPP-over-MCP integration available in InFlow
Node is available here. InFlow Go does not define a competing transport while upstream support
is absent. See the [compatibility notes](#upstream-compatibility-notes) for the package reference.

### Composing an x402 client

The `x402` package aliases the upstream V2 payment types, so they can cross the package boundary
without conversion. The `x402/buyer` client adds InFlow-managed payment creation and approval
waiting. It can also use an upstream client supplied through `Options.External` for external-wallet
payments. That client's registered schemes and spending controls remain active.

| Situation | InFlow behavior | What your application needs to do |
| --- | --- | --- |
| Both InFlow and an external wallet can pay | Matching managed requirements take precedence; external signing is the fallback when none match. | Configure `Prefer` for the managed scheme order. Use the upstream client directly for an external-wallet-only flow. |
| An external wallet is supplied to the combined client | The client still loads InFlow account capabilities. | Supply InFlow authentication; an external wallet does not make this combined client anonymous. |
| An application after-hook returns an error | Wrapper hooks propagate it on both payment routes. Upstream x402 Go v2.27.0 discards errors from its own after-hooks. | Register application callbacks through `Options.Hooks` when their errors must reach the caller. Hooks registered on `External` retain upstream behavior. |
| The paid HTTP response is another `402` | `Do` returns it without creating a second payment. It does not run upstream callbacks that update payment state from the seller's response. | Inspect the response before trying another payment. For an external-wallet scheme that requires those callbacks, use the [upstream HTTP client](#application-hooks-and-http-requests) instead of InFlow's `Do`. |

These are wrapper behaviors, not changes to the x402 wire protocol. The upstream after-hook
behavior is visible in its [v2.27.0 client implementation](https://github.com/x402-foundation/x402/blob/go/v2.27.0/go/client.go#L811-L817).
See [x402 Buyer](#x402-buyer) for setup and opt-in external-wallet sponsorship.

### Waiting, retrying, and cancelling approvals

An InFlow payment can require a person's approval. `Prepare` creates the payment and returns a
`Payment` object; `Wait` waits for its credential or payload. The two clients intentionally differ in what
a failed wait means. This is an SDK lifecycle choice, not a requirement of MPP or x402.

| Operation | After a failed wait | What your application needs to do |
| --- | --- | --- |
| MPP `Payment.Wait` | The payment object stores the error and attempts approval cancellation for up to five seconds. Later waits return that error. Cancelling any waiter abandons the payment for all waiters. | Do not call `Wait` expecting it to resume. Inspect the outcome before deciding to start another payment. |
| x402 `Payment.Wait` | The approval is not automatically cancelled. A later wait can poll the same transaction. | Keep the handle and call `Wait` with a fresh context to resume, or call `Cancel` to abandon it. Resuming does not guarantee that the server will approve the payment. |
| x402 `Client.Sign` | This one-shot operation attempts approval cancellation for up to five seconds after a failed wait. | Use `Prepare` and `Wait` instead when your application needs to resume waiting. |

For MPP, cancelling the context passed to `Prepare` cancels the payment operation, including
subsequent waiting. Its `WaitTimeout` starts when creation returns, even if `Wait` has not been
called yet. For x402, the preparation context applies only until `Prepare` returns; `WaitTimeout`
starts separately for each new polling attempt. Concurrent x402 waits share the first caller's
polling context. Cancelling a later caller stops only that caller's wait;
cancelling the first stops the shared attempt, but a later
attempt can resume it.

Once an x402 payload is received, the handle retains it and the result of its after-hooks. An
after-hook failure is reported without cancelling the ready payment or rerunning the hook on
another wait. Receiving a payload does not itself prove the seller accepted or settled it.
Neither protocol's cancellation operation reverses a completed payment, and best-effort approval
cleanup can fail. Inspect uncertain outcomes before starting another purchase.

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

### x402 Buyer

Import `github.com/inflowpayai/inflow-go/x402/buyer` to obtain managed payments or compose an
external-wallet client from `github.com/x402-foundation/x402/go/v2`. `New` performs no requests.
`Supported` loads the account's supported scheme/network pairs and caches them for one hour.
Concurrent loads share one request; a failed load can be retried.

The combined Buyer client needs an InFlow API key or OAuth access token for that capability lookup:
use [Sandbox](https://sandbox.inflowpay.ai) for testing or [Production](https://app.inflowpay.ai) for
live payments. Supplying an external wallet does not bypass this lookup. For external-wallet-only
payments without an InFlow account, use the upstream client directly; the opt-in sponsorship
extension below can also be registered on that client.

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
payment, err := client.Sign(ctx, required, buyer.SignOptions{})
```

`required` is the seller's decoded V2 `PaymentRequired`. `Sign` prefers managed `balance`, then
managed `exact`. Set `Prefer` to change that order. Policies filter candidates before selection.
When several balance assets match, a fresh account balance lookup can select an affordable asset;
an unavailable balance lookup falls back to the first match. InFlow remains the authority on
available funds. Managed signing does not support Permit2.

Set `External` to a configured upstream client for requirements that do not match managed
capabilities. Its registered schemes and spending limits remain in effect. A policy rejection
does not bypass the policy by switching to the external path. `SignOptions.PaymentID` and
`TransactionRequestExtensions` apply to managed signing. The returned `EncodedPayload` preserves
the server's signed encoding; use it directly as `PAYMENT-SIGNATURE`.

#### Waiting for approval

Use `Select` to choose a managed requirement, then pass a `PaymentRequired` containing that one
requirement to `Prepare`. The returned `Payment` exposes `ApprovalID`, `TransactionID`, `Status`,
`Wait`, and `Cancel`. Preparation creates the transaction once and does not poll.

```go
selected, err := client.Select(ctx, required)
if err != nil {
    return err
}
if selected == nil {
    return errors.New("no InFlow-managed payment option matches this request")
}
selectedRequired := required
selectedRequired.Accepts = []x402.PaymentRequirements{*selected}
pending, err := client.Prepare(ctx, selectedRequired, buyer.SignOptions{})
if err != nil {
    return err
}
waitContext, stopWaiting := context.WithTimeout(ctx, 30*time.Second)
payment, err := pending.Wait(waitContext)
stopWaiting()
```

This example uses `errors`, `time`, and the `x402` package alongside the Buyer client.
`required` is the seller's decoded `PaymentRequired`. A nil selection means no managed option
matches; `Prepare` is for managed payments only. Use `Sign` with a configured `External` client
when you want the external-wallet fallback.

If this wait times out, keep `pending` and call `pending.Wait` with a fresh context to resume the
same approval. To abandon it, call `pending.Cancel` with a live context. A timeout does not imply
that the payment failed or that its approval was cancelled. Each wait attempt has its own
`WaitTimeout` budget, defaulting to fifteen minutes; polling defaults to five seconds.

See [Waiting, retrying, and cancelling approvals](#waiting-retrying-and-cancelling-approvals)
for concurrent waits, after-hook failures, and the difference from MPP's payment lifetime.

#### Application hooks and HTTP requests

Configure application callbacks through `Options.Hooks`. Before-hook errors stop payment creation.
After-hook errors propagate on both managed and external-wallet paths. A failure hook can supply a
replacement payload for one-shot signing, but not for a prepared payment tied to existing approval
and transaction identifiers. Hooks registered directly on the upstream client retain upstream
behavior, including its non-fatal after-hook errors. Callbacks must honor their contexts and support
concurrent operations. Hook inputs are independent copies of payment data.

`Do(request, options)` sends an unpaid request and at most one paid replay. It preserves application
authentication, does not attach InFlow API credentials to the resource request, and never follows
redirects. A nonempty body must have `Request.GetBody`; replayability is checked before signing.
The caller closes the returned response body. `Do` returns a second 402 to the caller rather than
automatically authorizing another payment.

Some external-wallet payment schemes keep state that must be updated after reading the seller's
response. InFlow's `Do` does not call those upstream response callbacks. For those schemes, use
`Newx402HTTPClient(external)` and `WrapHTTPClientWithPayment` from
[`github.com/x402-foundation/x402/go/v2/http`](https://github.com/x402-foundation/x402/blob/go/v2.27.0/go/http/client.go#L148)
instead. That upstream transport calls the scheme's response handlers and applies its own retry
and redirect behavior; InFlow's single-paid-request and no-redirect guarantees do not apply to it.

#### External-wallet sponsorship

EIP-2612 support belongs to the upstream EVM exact signer. Configure its read-contract and typed-data
signing capabilities; the upstream implementation can attach a permit when the seller advertises
EIP-2612 sponsorship and Permit2 allowance is insufficient.

For InFlow's EIP-7702 sponsorship, import the opt-in `x402/buyer/eip7702` package and register its
extension on the upstream client. The main Buyer package does not import its blockchain dependencies.

```go
extension, err := eip7702.New(eip7702.Options{
    Environment: inflow.Sandbox,
    Signer: wallet,
    Consent: confirmDelegation,
})
if err != nil {
    return err
}
external.RegisterExtension(extension)
```

`wallet` implements `eip7702.Signer`; `confirmDelegation` receives the context and delegation
authorization and returns consent or an error. Delegation can persist even if the purchase fails,
so obtain the owner's consent explicitly. The extension only handles advertised exact Permit2
payments. It checks allowance, uses the caller-configured InFlow preparation endpoint, verifies
the exact approval/settlement batch and pinned contracts, and signs only after validating the
operation hash. It never broadcasts. The signer must support concurrent calls and must honor
cancellation. `SignMessage` applies Ethereum's personal-message prefix to the operation hash;
it must not sign the hash as a raw transaction digest.

### x402 Seller

Use `x402/seller` with the upstream x402 `net/http` middleware. InFlow supplies Seller
configuration, priced payment offers, scheme registrations and a facilitator client. The
upstream middleware checks payment, runs your handler and settles a successful response.
You do not need to implement or host a facilitator.

Create an InFlow **Seller** account in [Sandbox](https://sandbox.inflowpay.ai) or
[Production](https://app.inflowpay.ai), then create an API key in that dashboard. Set the
matching `Environment`. A Developer account cannot load Seller configuration.

```go
import (
    "context"
    "errors"
    "net/http"
    "os"

    inflow "github.com/inflowpayai/inflow-go"
    "github.com/inflowpayai/inflow-go/x402/seller"
    foundation "github.com/x402-foundation/x402/go/v2"
    xhttp "github.com/x402-foundation/x402/go/v2/http"
    "github.com/x402-foundation/x402/go/v2/http/nethttp"
)

func paidHandler(ctx context.Context, handler http.Handler) (http.Handler, error) {
    options := inflow.Options{Environment: inflow.Sandbox, APIKey: os.Getenv("INFLOW_API_KEY")}
    client, err := seller.New(options)
    if err != nil { return nil, err }
    facilitator, err := seller.NewFacilitator(options)
    if err != nil { return nil, err }
    route, err := client.Route(ctx, seller.RouteOptions{
        AcceptsOptions: seller.AcceptsOptions{Price: seller.PriceSpec{Amount: "$0.01"}},
    })
    if err != nil { return nil, err }
    if len(route.Accepts) == 0 { return nil, errors.New("no payment offers match this route") }
    registrations, err := client.SchemeRegistrations(ctx, seller.RegistrationOptions{})
    if err != nil { return nil, err }
    server := xhttp.Newx402HTTPResourceServer(
        xhttp.RoutesConfig{"GET /api/data": route},
        foundation.WithFacilitatorClient(facilitator),
    )
    for _, registration := range registrations {
        server.Register(registration.Network, registration.Server)
    }
    if err := server.Initialize(ctx); err != nil { return nil, err }
    return nethttp.PaymentMiddlewareFromHTTPServer(
        server, nethttp.WithSyncFacilitatorOnStart(false),
    )(handler), nil
}
```

This protects `GET /api/data`; unmatched routes still reach your handler without payment.
Initialize before starting your HTTP server. The explicit initialization returns configuration
errors to your application instead of delegating startup error handling to the middleware.
Supply a startup context with a deadline. The middleware buffers handler responses and is not a
streaming adapter. It does not settle ordinary authorization payments after a handler error or
panic, and it withholds successful content if settlement fails. It cannot undo work your handler
already performed, so make side effects idempotent using the payment identifier where appropriate.

`New` performs no requests. `Config` loads configuration on demand and caches it for one hour;
`RefreshConfig` forces a refresh. `SignerAddresses` uses the supported-capabilities cache and
matches an exact network before its namespace wildcard. `RefreshSupported` refreshes that cache.
Concurrent loads share a request; cancelling a joining caller stops only its wait. Cancelling
the caller that started the request fails that shared load, and another call can try again.
Returned configuration is independent of the cache. Refreshing configuration does not rebuild
an existing middleware instance: rebuild its offers and registrations when adopting changes.

`Accepts` constructs payment offers without sponsorship declarations. `Route` also checks
sponsorship eligibility. `PriceSpec.Amount` accepts `$0.01`, `0.01 USDC`, or `0.01` with an
explicit `Currency`. `Currency` overrides a currency in the amount string. `USD` selects all
configured stablecoins. Amounts allow at most eight decimal places; conversion to atomic units
rejects nonzero precision loss. `Schemes` and `Networks` filters intersect; nil means unrestricted,
while an empty slice selects nothing. The default payment lifetime is 300 seconds.

#### Metered payments

Metered `upto` payments require explicit selection. Import the upstream implementation only in
applications that need it; fixed-price sellers do not compile Ethereum packages through InFlow's
seller package. This is a Go setup difference from Node's dynamically loaded optional EVM package.

```go
import upto "github.com/x402-foundation/x402/go/v2/mechanisms/evm/upto/server"

offers, err := client.Accepts(ctx, seller.AcceptsOptions{
    Price: seller.PriceSpec{Amount: "0.10 USDC"},
    Schemes: []string{"upto"},
})
if err != nil { return err }
registrations, err := client.SchemeRegistrations(ctx, seller.RegistrationOptions{
    Schemes: []string{"upto"},
    MeteredScheme: upto.NewUptoEvmScheme(),
})
if err != nil { return err }
```

Use `offers` as the route's `Accepts`, and register the returned schemes as in the full example.
Check for no offers before starting. Configuration must advertise the asset's Permit2 capability
and the network's metered proxy and facilitator address. Selecting an available metered scheme
without supplying `MeteredScheme` returns an error rather than silently skipping its registration.

The advertised price is the buyer's authorized maximum. In your handler, call
`nethttp.SetSettlementOverrides(w, &foundation.SettlementOverrides{Amount: "123"})` before writing
the response to charge 123 atomic units of the selected asset. Choose an integer amount between
zero and the authorized maximum; without an override, settlement uses that maximum. This path
uses an external blockchain wallet, not InFlow-managed Permit2 signing.

#### Sponsorship and facilitator access

Set `RouteOptions.AssetTransferMethod` to `"permit2"` to select compatible Permit2 offers.
Balance offers remain available unless filtered out. `Route` declares EIP-2612 sponsorship only
when every Permit2 offer supplies the required token metadata and the refreshed facilitator
capabilities agree. Otherwise it checks explicit InFlow EIP-7702 sponsorship support. Missing
capability information never implies support. Use separate routes for incompatible tokens.
For multiple facilitators, put InFlow first for routes whose sponsorship it advertises; the
upstream middleware selects the first facilitator claiming a scheme/network pair.

`NewFacilitator` requires an API key and implements upstream `FacilitatorClient` directly.
`NewAnonymousFacilitator` explicitly sends no credentials, even if options contain them. Anonymous
facilitation does not load Seller configuration and cannot settle InFlow balance payments.
`Verify` and `Settle` accept the upstream interface's JSON byte slices. Verification does not
settle. False verification or settlement results remain false results; unrelated HTTP failures
remain `inflow.APIError`. Only the recognized Permit2 allowance response is normalized from
HTTP 412 into a verification result.

The facilitator preserves a valid payment identifier or derives one from the transaction ID,
serialized transaction or signature. If none exists, it hashes the compact JSON payment data;
retain the same payload bytes for verification and settlement in that fallback case. Requests
preserve unknown payload fields and extensions. Only HTTP 409 `idempotency_pending` retries:
five total attempts, reusing the same request, with a cancellable delay of up to five seconds.
Other failures do not trigger automatic payment retries. Cancellation stops waiting; it does
not prove that an already submitted payment was reversed.

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

Paid responses include `Cache-Control: private` so shared caches must not reuse them for other
users. `Protect` preserves your handler's other cache directives and adds `private` when necessary.
This does not prohibit browser-local caching; set `Cache-Control: no-store` in your handler when
responses must not be stored.

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

2. **Checking request replay before payment.** The upstream transport creates a payment credential before checking whether the request body can be replayed. For a body without `GetBody`, this can invoke the payment method and then fail locally without sending the paid request. InFlow checks that it can recreate the body before requesting payment. Supply `Request.GetBody`; `http.NewRequest` supplies it automatically for `strings.Reader`, `bytes.Reader`, and `bytes.Buffer`. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/client/transport.go#L92-L125).

3. **Separate validation and broadcast.** The upstream seller `Intent` interface exposes one `Verify` operation returning a receipt. InFlow's `Validate` checks a credential without consuming payment; `Broadcast` submits it for payment processing. Keeping them separate lets an application check a credential without unexpectedly processing a payment. Use InFlow's `Verify` to perform both, or `Protect` to perform both before running an HTTP handler. [Source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L17-L27).

4. **Subscription entry points.** The upstream generic verification function accepts an arbitrary intent, but the `Charge` helper selects `charge` explicitly, and `ComposeMiddleware` operates on charge configurations. This limits those convenience APIs, not the protocol's ability to encode subscription challenges. InFlow sellers advertise subscriptions with `seller.Offer.Subscription`. Buyers use `Fulfil` to purchase one, or supply `PaymentOptions.SubscriptionID` to authorize access under an existing subscription. [Charge source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L148-L164), [composition source](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/compose.go#L12-L37).

5. **Preserving receipt fields.** The upstream receipt type has a fixed set of fields and a nested `extra` object. InFlow receipts also carry top-level fields such as `challengeId`, `subscriptionId`, and `settlement`. The upstream receipt parser/formatter does not preserve those top-level fields; moving them into `extra` changes the wire format. Use InFlow's `DecodeReceipt` and `EncodeReceipt` to preserve them. [Type](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/receipt.go#L7-L14), [parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L502-L537).

6. **Server dependency coupling.** The upstream server package imports its Tempo package, which brings Ethereum, Tempo, and Redis packages into the compilation dependencies. InFlow delegates payment processing to its platform and does not need that entire server implementation for this purpose. Using the protocol primitives avoids this coupling. The upstream module's web-framework requirements do not mean every framework is compiled into every consumer. This distinction was checked with `go list -deps`. [Server imports](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/server/server.go#L9-L15), [Tempo Redis dependency](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/tempo/redis_store.go#L3-L8).

7. **MPP over MCP is not supported.** The released Go library has no MCP integration package corresponding to the `mppx/mcp/client` integration used by InFlow Node. InFlow Go does not implement an independent MPP-over-MCP transport. Support depends on an upstream implementation so that integrators do not adopt an InFlow-specific design that could conflict with the upstream protocol integration. This limitation concerns MPP, not x402's separate MCP integration. [Released package tree](https://github.com/tempoxyz/mpp-go/tree/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg).

8. **Preserving opaque challenge data.** The upstream challenge parser decodes `opaque` into a string map, and its formatter encodes that map again. An issuer's original encoded value can therefore change. InFlow keeps that field as its original string and uses its own wire types, while reusing compatible upstream header primitives. [Parser and formatter](https://github.com/tempoxyz/mpp-go/blob/41c35ed9e9332b9d224c1fc4af606efa98a24251/pkg/mpp/parse.go#L213-L298).

## Development

Requires Go 1.26 or later and Make. Run `make verify` for formatting, module tidiness, compilation,
static analysis, race-enabled tests, package documentation, and a build from a separate consumer
module. `make format` formats Go source files.

CI runs on Go 1.26 and 1.27. Local tests require at least 99% statement coverage in every source
file. Codecov evaluates 99% project and changed-line targets without tolerance. Its line-based
measurements differ from Go's statement coverage; aim for 100% on both.

The consumer check builds and runs a separate Go module against the local checkout. It checks
configuration and error types independently of payment behavior.

### Node interoperability

The `Node interoperability` workflow runs real HTTP exchanges in both directions: a Go Buyer
against Node Seller middleware and a Node Buyer against Go Seller middleware. It covers MPP
charges, subscription purchases, existing-subscription authorization, Tempo charges, and x402
balance and exact payments. Cases include success, pending approvals where applicable, rejected
validation, failed settlement, and failed application handlers. Assertions check payment data,
receipts, platform credential isolation, and handler/settlement ordering.

The payment platform is a loopback simulator. These tests do not sign real transactions, move
money, or certify blockchain settlement. The Node MPP fetch client is configured for one payment
attempt (`maxPaymentRetries: 1`); this prevents its automatic retries from purchasing again after
a rejected paid request. Existing-subscription authorization returns a credential directly and
does not have a pending-purchase case.

For a local run, use Node 24 and a clean `inflow-node` checkout at the revision in
`interop/node.lock.json`. In that checkout run `pnpm install --frozen-lockfile` and `pnpm build`,
then run this command from the Go repository:

```sh
node scripts/interoperability.mjs /path/to/inflow-node /tmp/inflow-interoperability.json
```

Choose an unused report filename; the runner refuses to overwrite an existing report. Reports
record SDK revisions, toolchain versions, Go dependencies, and each case's platform requests.
Hosted reports are available from the workflow's artifacts. The peer programs live in `interop/`
and are test tooling, not SDK packages intended for application use.

### Releases

Go versions come from immutable Git tags such as `v0.1.0`; there is no separate version constant
to update. Install a release with `go get github.com/inflowpayai/inflow-go@v0.1.0`, replacing the
example version with the release you want. All packages in this repository share that version.
The SDK user-agent reads the installed module version. Local `replace` builds report `devel`.

Maintainers use [Actions → Release](https://github.com/inflowpayai/inflow-go/actions/workflows/release.yml):

1. Select **Run workflow**, choose **main**, and enter a stable version without `v`.
2. Leave **dry_run** checked. The workflow runs the repository checks, a separate local consumer,
   shared conformance, and Node interoperability. Download `inflow-go-release-evidence` from the
   completed run to inspect the reports. A dry run does not create a tag or GitHub release.
3. After reviewing the evidence and approving publication, run the workflow from **main** with the
   same version and **dry_run** unchecked. It verifies the selected commit again, creates an
   annotated tag, installs that version through `proxy.golang.org` in a separate consumer module
   without a local replacement, and publishes the GitHub release with its verification reports.

No registry account, API key, or repository secret is required. The workflow uses GitHub's provided
token. Merging a PR or pushing a tag does not trigger publication. Release-workflow PRs exercise
the dry-run path automatically, using `0.1.0` only as a validation example.

Before version 1.0, use a minor increment for incompatible public API changes and a patch increment
for compatible fixes. From version 1.0, use semantic versioning. This module path supports major
versions 0 and 1; version 2 requires a `/v2` module path and a separate migration.

If publication fails after creating the tag, keep the tag: consumers may already have downloaded
it. Re-run the failed workflow jobs on the same commit after resolving the failure. The workflow
accepts an existing tag only when it points to that exact commit and refuses to replace an existing
GitHub release. If the code must change, choose a new version. Go proxy propagation can delay the
published-consumer check; do not delete or move a tag to retry it.

Release evidence records the source commit, contract and Node revisions, toolchain, dependency
versions, report checksums, and workflow run. These are traceability records, not signed build
attestations. The tests simulate payment processing and do not certify live settlement.
