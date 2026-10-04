# Verify a TAP-signed HTTP request

This server demonstrates agent recognition through Visa Trusted Agent Protocol.
It does not charge a payment, authenticate a buyer, or need an InFlow account.

From the repository root:

```sh
PUBLIC_ORIGIN=http://127.0.0.1:3001 go run ./examples/tap-seller
```

An unsigned request is rejected:

```sh
curl -i http://127.0.0.1:3001/example
```

Expect HTTP 401 and `TAP verification failed`. A successful request must carry a
real Ed25519 signature from a key available through the configured trusted resolver.
The example uses Visa's key service; it does not provide a production signing key.
The automated example tests sign synthetic requests and verify both body-bearing
and explicitly empty-body requests without contacting Visa:

```sh
go test -race ./examples/tap-seller
```

On success the server returns HTTP 200 and `Verified agent request (browse)` or
`Verified agent request (pay)`. The intent describes the signed request, not
customer consent or proof of payment.

## Adapting the example

- Set `PUBLIC_ORIGIN` to the public HTTP or HTTPS origin, without a trailing slash,
  path, credentials, query, or fragment. Set `LISTEN_ADDR` separately if the process
  listens elsewhere. The example ignores forwarding headers supplied by callers.
- Preserve the signed path and query through your proxy. A deployment serving several
  public origins must map requests to an explicitly trusted origin before verification.
- Read exact body bytes before parsing JSON. This example limits bodies to 1 MiB and
  returns HTTP 413 when that limit is exceeded. A signed digest also identifies an
  explicitly empty body, which still requires verification.
- Reuse the verifier across requests. Its default replay store protects only one
  process; multiple instances need a shared atomic `ReplayStore`.
- Put application authorization and any MPP or x402 processing inside the verified
  callback before serving a paid resource. TAP recognition does not replace those checks.

The handler deliberately returns a generic HTTP 401 for verification failures.
Your application can distinguish `*seller.Error` codes to choose its own response
policy, including a temporary key-service outage. Do not log request bodies or
signature material indiscriminately.
