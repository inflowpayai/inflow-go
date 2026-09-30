# x402 buyer

Follow the [account setup](../README.md#before-you-start), then start the
[x402 seller](../x402-seller/README.md) in another terminal.

```sh
export INFLOW_API_KEY='your-sandbox-buyer-api-key'
go run ./examples/x402-buyer
```

The default resource is `http://127.0.0.1:3001/api/widgets`, priced at 0.01 USDC.
The example selects an available InFlow-managed offer, waits for approval when required, and sends
one paid request. Approve in the Sandbox dashboard if requested. Successful paid output includes
HTTP 200, widgets JSON, and the decoded seller settlement result. For a balance payment,
output looks like this (the transaction varies per payment):

```text
Requesting resource; approve in InFlow if requested.
HTTP 200
{"widgets":[1,2,3]}

Seller settlement: success=true network=inflow:1 transaction=<transaction-reference>
```

Approval takes place in [Sandbox Approvals](https://sandbox.inflowpay.ai/approvals/), not in this
terminal. An already-authorized payment can complete without a new approval prompt.
The settlement summary reports the seller's response; decoding is not independent verification.

```sh
TARGET_URL='http://127.0.0.1:3001/free' go run ./examples/x402-buyer
```

A free response needs no payment or settlement header. A rejected paid response returns an error;
the program does not automatically pay again. It does not configure an external wallet or opt into
delegation. See [external-wallet sponsorship](../../README.md#external-wallet-sponsorship) for that
separate integration.

## Manual waiting and cancellation

`Do` already waits for approval. Use `Select`, `Prepare`, and `Wait` to retain a handle and resume
waiting after a timeout. Given the seller's decoded `PaymentRequired` named `required`:

```go
selected, err := client.Select(ctx, required)
if err != nil {
    return err
}
if selected == nil {
    return errors.New("no matching InFlow-managed offer")
}
required.Accepts = []x402.PaymentRequirements{*selected}
pending, err := client.Prepare(ctx, required, buyer.SignOptions{})
if err != nil {
    return err
}
fmt.Println("Approval:", pending.ApprovalID())
waitContext, stopWaiting := context.WithTimeout(ctx, 30*time.Second)
payment, err := pending.Wait(waitContext)
stopWaiting()
if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
    // Resume the same transaction using the still-live application context.
    fmt.Println("The wait timed out; resuming the same payment without creating another.")
    payment, err = pending.Wait(ctx)
}
if err != nil {
    return fmt.Errorf("payment is not ready: %w", err)
}
fmt.Println("Payment payload ready for transaction:", payment.TransactionID)
```

After a timeout, keep `pending` and use a fresh context to wait on the same transaction.
To abandon the approval, use a live, bounded context:

```go
cancelContext, stopCancellation := context.WithTimeout(context.Background(), 5*time.Second)
defer stopCancellation()
if err := pending.Cancel(cancelContext); err != nil {
    return fmt.Errorf("approval cancellation could not be confirmed: %w", err)
}
```

Cancellation can fail and does not reverse payment. The one-shot `Do` path uses `Sign`, which
attempts approval cleanup after a failed wait; manual `Wait` keeps the approval available for
resuming. These are deliberately different workflows.

The returned `payment.EncodedPayload` belongs in the seller request's `PAYMENT-SIGNATURE` header.
Receiving it does not establish settlement. Preserve the resource request and inspect the seller's
response, or use `Do` for the complete HTTP exchange.
