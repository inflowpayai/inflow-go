# x402 seller

Use an API key from an InFlow **Seller** account in
[Sandbox](https://sandbox.inflowpay.ai). A Developer key cannot load seller configuration.
Run from the repository root:

```sh
export INFLOW_API_KEY='your-sandbox-seller-api-key'
go run ./examples/x402-seller
```

The server binds to `127.0.0.1:3001`.

Authorization is the default. To settle before the handler runs, start it with:

```sh
X402_PAYMENT_FLOW=upfront go run ./examples/x402-seller
```

`X402_PAYMENT_FLOW=authorization` explicitly selects the default; other nonempty values fail
startup. With upfront, an unsuccessful or uncertain settlement prevents handler execution.
If the handler fails after successful settlement, the payment remains settled and its receipt
is retained. There is no automatic refund. Choose upfront for fixed-price work that must be paid
before it starts; choose authorization when a failed handler should not be charged.

| Route | Result |
| --- | --- |
| `GET /api/widgets` | 0.01 USDC through configured balance or exact offers |
| `GET /free` | Free response |

The example loads configuration, builds offers and scheme registrations, and initializes the
upstream x402 middleware before listening. It stops if configuration fails or no matching offer
is available. Seller configuration determines networks and recipients; the example does not
invent wallet addresses.

Inspect without paying:

```sh
curl -i http://127.0.0.1:3001/api/widgets
```

The server prints a timestamped `x402 seller listening on http://127.0.0.1:3001` message.
The unpaid response includes these fields (encoded content varies; other headers omitted):

```http
HTTP/1.1 402 Payment Required
Payment-Required: <base64-encoded payment requirements>
```

This 402 is the expected payment offer, not a failed server startup.
Run the [x402 buyer](../x402-buyer/README.md)
in another terminal with a separate buyer key to pay. In authorization mode, the upstream middleware verifies payment,
runs the handler, and settles a successful response before releasing it. Settlement failure
withholds the successful response, but cannot undo application side effects. This handler only
returns JSON.

In either mode, make application fulfillment safe to repeat: payment deduplication does not stop
the handler from running again for the same payment. Keep the original payment when retrying;
do not create another purchase because the application returned an error.

Only the configured route is payment-protected. `/free` is deliberately unprotected; unknown
routes return 404. Add application authentication outside this middleware when needed.

This example uses fixed prices. [Metered payments](../../README.md#metered-payments) need the
explicit upstream metered implementation. `Route` builds offers from Seller configuration and
can advertise [sponsorship](../../README.md#sponsorship-and-facilitator-access) when its selected
Permit2 offers and facilitator capabilities support it. The paired buyer example uses managed
payments; external-wallet Permit2 signing requires a separately configured wallet client.
