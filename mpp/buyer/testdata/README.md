# MPP Buyer fixtures

`buyer.json` contains all 25 `mpp-buyer` cases from
[`inflow-specs` revision f200337](https://github.com/inflowpayai/inflow-specs/blob/f20033742244dc4220e0cdc05db1e1eeeaccdb63/fixtures/mpp.mjs).
`TestSharedBuyerCases` runs each through the public Buyer API against real local HTTP endpoints,
checking the request sequence, bodies, credentials, problems, timeout, and approval cancellation.
Native tests additionally exercise in-flight cancellation and concurrent waits. These tests use
synthetic credentials; they do not demonstrate live settlement or server ownership enforcement.
