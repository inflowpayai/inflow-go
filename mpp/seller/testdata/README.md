# MPP Seller fixtures

`seller.json` contains all 40 Seller cases from the InFlow shared MPP corpus at
[`f200337`](https://github.com/inflowpayai/inflow-specs/blob/f20033742244dc4220e0cdc05db1e1eeeaccdb63/fixtures/mpp.mjs).
Tests invoke public Seller methods and the HTTP middleware against local HTTP servers.
They verify preparation, validation, broadcast, idempotency, preserved problems, and route binding.
Synthetic credentials do not establish live settlement or server-side account ownership.

The native challenge-binding test includes an independently generated challenge ID from
`mppx` 0.8.17, using its actual `Challenge.from` implementation and synthetic signing key.
