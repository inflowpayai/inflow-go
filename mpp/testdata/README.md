# MPP core fixtures

`core.json` contains the 18 `mpp-core` cases exported from
[`inflow-specs` revision f200337](https://github.com/inflowpayai/inflow-specs/blob/f20033742244dc4220e0cdc05db1e1eeeaccdb63/fixtures/mpp.mjs).
`TestSharedCoreCases` executes every case through the public Go codecs. Fixture identifiers and
credentials are synthetic. These tests do not perform payments or certify Buyer/Seller workflows.
