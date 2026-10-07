# MPP buyer

Follow the [account setup](../README.md#before-you-start), then start the
[MPP seller](../mpp-seller/README.md) in another terminal.

```sh
export INFLOW_API_KEY='your-sandbox-buyer-api-key'
go run ./examples/mpp-buyer
```

The default resource is `http://127.0.0.1:3000/api/widgets`, priced at 0.01 USDC.
`buyer.Client.Do` reads the challenge, obtains a credential through InFlow, waits for approval,
and sends the paid request. Approve in the Sandbox dashboard if requested. Successful output
includes HTTP 200, the resource body, and the decoded seller receipt. For example
(the reference varies per payment):

```text
Requesting resource; approve in InFlow if requested.
HTTP 200
{"ok":true,"message":"Paid resource accessed"}

Seller receipt: status=success method=inflow reference=<payment-reference>
```

Approval takes place in [Sandbox Approvals](https://sandbox.inflowpay.ai/approvals/), not in this
terminal. An already-authorized payment can complete without a new approval prompt.

A free request needs no payment:

```sh
TARGET_URL='http://127.0.0.1:3000/free' go run ./examples/mpp-buyer
```

## Visa CARD

Start the Seller in [CARD mode](../mpp-seller/README.md#visa-card), then use the Buyer account's
linked Visa card with an enabled USD allowance for the purchase. Set the actual merchant details:

```sh
export MPP_PAYMENT_METHOD=card
export MERCHANT_NAME='Your merchant name'
export MERCHANT_URL='https://your-merchant.example'
export MERCHANT_COUNTRY_CODE='US'
go run ./examples/mpp-buyer
```

This makes a payment; it does not merely inspect the challenge. InFlow uses the primary instrument
unless you set `INSTRUMENT_ID` to a linked card's UUID. The server checks eligibility and allowance.
The example does not provision a card, create an allowance, or decrypt the returned credential.
CARD mode selects CARD offers; it does not fall back to a different payment method.
Do not set `SUBSCRIPTION_ID` for CARD. Unset `MPP_PAYMENT_METHOD` for the ordinary USDC example.

## Subscriptions

`/api/subscribe` offers a **recurring** 1.00 USDC monthly subscription, expiring one year after
the seller process starts. This command can purchase it; it is not an inspection command:

```sh
TARGET_URL='http://127.0.0.1:3000/api/subscribe' go run ./examples/mpp-buyer
```

To access it with an existing eligible subscription, obtain its identifier from InFlow and use:

```sh
SUBSCRIPTION_ID='your-subscription-id' \
TARGET_URL='http://127.0.0.1:3000/api/subscribe' \
go run ./examples/mpp-buyer
```

Cancelling an approval does not cancel the recurring subscription. Restarting the example seller
changes its advertised expiry; a real application should persist stable subscription terms.

## Manual waiting and cancellation

`Do` already handles approval. Use `Prepare` when your application needs to display the approval
identifier or provide its own cancellation control. Given a challenge selected from
`mpp.ParseChallenges` and your configured client:

```go
pending, err := client.Prepare(ctx, challenge, buyer.PaymentOptions{})
if err != nil {
    return err
}
fmt.Println("Approval:", pending.ApprovalID())
credential, err := pending.Wait(ctx)
if err != nil {
    return err
}
```

Keep `ctx` alive through preparation and waiting. An unsuccessful MPP wait is terminal for the
handle and attempts approval cancellation for up to five seconds. A separate Cancel button can
call `pending.Cancel` with a live context. Receiving `credential` does not send it to the seller:
encode it and send the paid resource request, or use `Do` for that full exchange.

To abandon a pending approval explicitly:

```go
cancelContext, stopCancellation := context.WithTimeout(context.Background(), 5*time.Second)
defer stopCancellation()
if err := pending.Cancel(cancelContext); err != nil {
    return fmt.Errorf("approval cancellation could not be confirmed: %w", err)
}
```

This requests approval cancellation, not a refund or subscription cancellation.

MPP occupies `Authorization`. For application authentication, use a separate header or cookie
supported by the service. The client rejects an existing Authorization value rather than replace
it. See [HTTP behavior](../../README.md#payment-aware-http-requests) for details.
