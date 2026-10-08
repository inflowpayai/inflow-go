# Runnable examples

For payment-independent agent recognition, see the [TAP Seller example](tap-seller/).
It needs a public origin configuration, not an InFlow API key.

The MPP and x402 programs use **InFlow Sandbox**, not a simulated payment platform. Sellers run locally,
but configuration, approvals, and payment processing use your Sandbox accounts.
Use Go 1.26 or later and run commands from the repository root.

## Before you start

1. Create accounts at [InFlow Sandbox](https://sandbox.inflowpay.ai). The seller needs a
   **Seller** account and an API key from its dashboard. A Developer key cannot accept payments.
   The buyer can use a Developer account; Seller accounts can also act as buyers. Use separate
   accounts and keys to make the two sides easy to follow.
2. In the buyer account, open [Sandbox Balances](https://sandbox.inflowpay.ai/balances/).
   For a balance-funded test, have at least **0.01 USDC** available for one widgets request,
   or **1.00 USDC** for the initial subscription payment. To fund the account, open
   [Deposit](https://sandbox.inflowpay.ai/transactions/deposit/), select USDC, then select an
   available blockchain to obtain its deposit address. Use test assets on the network configured
   for Sandbox, not mainnet funds. Wait until Balances shows the credited USDC before paying.
   If USDC or a deposit network is unavailable, resolve that account setup before continuing.
   An API key alone does not supply funds or guarantee approval.
3. Download dependencies with `go mod download`.
4. Keep the seller running in one terminal and run the buyer in another. Set `INFLOW_API_KEY`
   independently in each terminal.

| Protocol | Seller | Buyer |
| --- | --- | --- |
| MPP | [Start the MPP seller](mpp-seller/README.md) on port 3000 | [Run the MPP buyer](mpp-buyer/README.md) |
| x402 | [Start the x402 seller](x402-seller/README.md) on port 3001 | [Run the x402 buyer](x402-buyer/README.md) |

Each buyer makes one request. If it receives a supported payment challenge, it requests payment
through InFlow, waits for approval when required, and sends one paid request. Keep the Sandbox
dashboard available to approve the request. A free resource does not trigger payment.
A second 402 or another non-success response exits with an error instead of paying again.

## Settings

| Variable | Applies to | Meaning |
| --- | --- | --- |
| `INFLOW_API_KEY` | All | Sandbox API key; a Seller key for sellers |
| `TARGET_URL` | Buyers | Resource to request; defaults to the matching local seller's `/api/widgets` |
| `LISTEN_ADDR` | Sellers | Defaults to loopback port 3000 for MPP or 3001 for x402 |
| `MPP_SECRET_KEY` | MPP seller | Private challenge-signing key, separate from the API key |
| `MPP_PAYMENT_METHOD` | MPP seller and buyer | Seller: `inflow` (default), `stripe`, or `card`. Buyer: `card` selects Visa CARD with merchant context |
| `MERCHANT_NAME`, `MERCHANT_URL`, `MERCHANT_COUNTRY_CODE` | MPP CARD buyer | Purchase merchant name, absolute HTTP(S) URL and two-letter country code |
| `INSTRUMENT_ID` | MPP buyer | Optional linked instrument UUID; CARD otherwise uses the account's primary instrument |
| `SUBSCRIPTION_ID` | MPP buyer | Authorize access with an existing subscription instead of purchasing another |
| `INFLOW_BASE_URL` | All | Optional platform URL override for private deployments or local tests; leave unset for Sandbox |

The programs do not load `.env` files. Export variables in the shell. Never commit real keys.
Platform credentials are not sent to `TARGET_URL`. Only target services you intend to pay:
these examples are payment clients, not free inspection commands.
The sellers have no application login; add your application's authentication outside the payment
middleware when required.

Both buyers have a fifteen-minute overall deadline and respond to Ctrl-C. Cancellation does not
reverse a completed payment. Do not automatically rerun a command after an uncertain network
failure. See [waiting and cancellation](../README.md#waiting-retrying-and-cancelling-approvals).

## Results and verification

Buyers print the HTTP status, response body, and readable fields decoded from the seller's
`Payment-Receipt` (MPP) or `PAYMENT-RESPONSE` (x402). These are seller-reported results;
decoding does not independently verify settlement. A successful free response can have no
receipt; missing receipts are reported without claiming that no payment happened.
The programs do not print the submitted payment credential or signature.

MPP processes payment **before** the application handler runs. x402 settles a successful handler
response before releasing it. Neither makes application side effects transactional with payment.
The demonstration handlers only return content.

`make verify` builds all five programs, tests startup and HTTP behavior against local servers,
and runs race detection and coverage checks. Inert keys and scripted platform responses test
SDK integration, not live settlement. Instrumented executable startup coverage is combined with
unit coverage so the actual command entry points remain inside the coverage gate.

See each buyer's README for manual waiting and cancellation. These are alternatives to the
automatic waiting in `Do`, not separate payment capabilities.
