# Shared conformance

The adapters call the public Go SDK and compare its results with `inflow-specs`. They are test
files compiled into a separate, race-enabled test binary. They do not add dependencies to an
application using the SDK. Node.js runs the shared test runner, not payment code.

Use Go 1.26 or later, Node.js 24, and the pnpm version declared by `inflow-specs`. Prepare a clean
checkout of `inflow-specs` at the commit in [inflow-specs.lock.json](inflow-specs.lock.json), then
run `pnpm install --frozen-lockfile` in that checkout. From this repository:

```sh
node --test scripts/conformance.test.mjs
mkdir /tmp/inflow-go-reports
node scripts/conformance.mjs --contract-root ../inflow-specs --output-dir /tmp/inflow-go-reports
```

The output directory must exist and must not contain `runtime.json`, `mpp.json`, `x402.json`, or `tap.json`.
Existing reports are never overwritten. The command runs all four suites and fails if any case
fails; it retains failed reports too. A report is evidence only when `completed` and `passed` are
both true. Reports include the source commits and dirty states, case hashes, the compiled adapter's
Go version, and resolved module versions. An unreleased local module is identified as `(devel)`
alongside its exact source commit. Dependency metadata lists the selected Go module graph.

To check a different clean contract revision, pass `--contract-revision FULL_COMMIT_SHA`.
That changes the explicit test input; it does not update the pin or excuse failed cases.

## What the reports exercise

- **TAP:** real synthetic Ed25519 requests pass through the public verifier and
  verified callback. Cases cover canonical signature parameters, signed request
  tampering, body digests, time boundaries, concurrent nonce claims, resolver/store
  failures, and the built-in key cache using the runner's real loopback key server.
  Accepted and rejected requests are checked for input mutation. The adapter neither
  parses signatures nor implements its own verifier or HTTP key cache.

- **Runtime:** the four public Buyer/Seller clients select environments and preserve authentication
  errors, Seller-account rejection messages, correlation identifiers, and credential-header
  redaction. Creation is not retried; safe configuration reads can retry and obtain fresh tokens.
  Environment tests intercept the configured transport without connecting to production. Other
  runtime cases use real loopback HTTP. Redirects must not cause an extra request.
- **MPP:** every shared Core, Buyer, and Seller case. The SDK creates payments, waits, cancels,
  prepares offers, validates credentials, and broadcasts. Route-binding cases use `Protect`, not
  an adapter-written replacement. Go's minimum positive polling interval is used because zero
  selects its production default. Server polling advice and SDK timeouts remain authoritative.
- **x402:** every shared Core, Buyer, and Seller case. The SDK selects capabilities, creates and
  waits for payments, handles cancellation, builds offers, verifies, and settles. Go explicitly
  loads capabilities through `Supported`; constructors perform no network activity. Offer/route
  fixtures are served unchanged through local HTTP instead of Node's configuration-provider object.
  The cancel case observes `Wait` after `Cancel`, including a recognized API rejection of cleanup.
  The SDK's separately returned cancellation error remains covered by native tests.

The Go adapter does not expose or call the internal raw HTTP client to imitate Node's runtime
adapter. Runtime rejection fixtures are applied to the supported public endpoints; generic approval
read methods are not claimed. Payment suites test the SDK-owned approval lifecycle. Input-mutation
checks protect the supplied objects, and unknown errors fail rather than becoming expected outcomes.

The shared runner owns the two documented x402 empty-field serialization equivalences. The adapter
does not rewrite observations, signed payment bytes, or expected outcomes.

These synthetic tests do not move money or prove live signing, settlement, server replay enforcement,
or interoperability with another language. Native tests supplement them with HTTP middleware,
external-wallet, sponsorship, concurrency, and cancellation coverage. The cross-language Buyer/Seller
interoperability tests are a separate check; conformance is not a substitute for them.

## Hosted reports

The **shared conformance** workflow runs the pinned suites on Go 1.26 and 1.27 for pull requests,
main-branch pushes, and manual dispatch. Its `inflow-go-conformance-*` artifacts contain all available
reports, including failed reports, for 14 days. This workflow supplements `make verify`; it does not
replace formatting, vet, race tests, coverage thresholds, documentation, or consumer checks.
