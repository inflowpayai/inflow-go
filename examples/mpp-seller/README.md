# MPP seller

Use an API key from an InFlow **Seller** account in
[Sandbox](https://sandbox.inflowpay.ai). A Developer key cannot load seller configuration.
Run from the repository root:

```sh
export INFLOW_API_KEY='your-sandbox-seller-api-key'
export MPP_SECRET_KEY="$(openssl rand -hex 32)"
go run ./examples/mpp-seller
```

The signing key is private and distinct from the API key. Keep it stable across instances of a
real service; changing it invalidates outstanding challenges. This command generates a local
demonstration key.

| Route | Result |
| --- | --- |
| `GET /api/widgets` | 0.01 USDC charge |
| `GET /api/subscribe` | Recurring 1.00 USDC monthly subscription, expiring one year after startup |
| `GET /free` | Free response |

The server binds to `127.0.0.1:3000`. Startup loads Seller configuration and checks that its
currency and subscription offers can be prepared. A rejected key or unsupported offer stops startup.

Inspect a challenge without paying:

```sh
curl -i http://127.0.0.1:3000/api/widgets
```

The server prints a timestamped `MPP seller listening on http://127.0.0.1:3000` message.
The unpaid response includes these fields (challenge values vary; other headers omitted):

```http
HTTP/1.1 402 Payment Required
Www-Authenticate: Payment id="<challenge-id>", ...
Cache-Control: no-store

Payment required
```

This 402 is the expected payment offer, not a failed server startup.
Run the [MPP buyer](../mpp-buyer/README.md)
with a separate buyer key to complete the exchange. Trying the subscription endpoint is optional;
its recurring terms are not a one-time charge.

`Protect` verifies challenge binding, validates the credential, and broadcasts payment before
running the handler. A handler failure does not reverse payment. Each route has a distinct signed
opaque value so its credential cannot be moved to another route.

This example has no application authentication. Place application authentication outside `Protect`,
using a separate API-key header or cookie where MPP needs Authorization. See the
[SDK integration guide](../../README.md#accepting-payments) for instruments, alternative offers,
Tempo, and separate validation/broadcast calls.
