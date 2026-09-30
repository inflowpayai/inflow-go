import { test } from "node:test";
import assert from "node:assert/strict";
import { runtimeCases } from "../conformance/runtime-cases.mjs";
import { checkContract } from "./conformance.mjs";

test("runtime cases use the four public endpoints and do not mutate fixtures", () => {
  const fixtures = {
    "auth.invalid-key": {
      exchanges: [
        {
          request: { headers: { "x-api-key": "test-only-key" } },
          response: { status: 401 },
        },
      ],
    },
    "auth.expired-bearer": {
      exchanges: [
        {
          request: { headers: { authorization: "Bearer test-only-token" } },
          response: { status: 401 },
        },
      ],
    },
    "auth.seller-required-developer-key": {
      exchanges: [
        {
          request: { headers: { "x-api-key": "test-only-key" } },
          response: {
            status: 403,
            json: {
              errors: [
                { code: "SELLER_ACCOUNT_REQUIRED", message: "Seller required" },
              ],
            },
          },
        },
      ],
    },
    "approval.cancel": { exchanges: [] },
    "auth.success": {
      exchanges: [{ request: { headers: {} }, response: { status: 200 } }],
    },
  };
  const before = structuredClone(fixtures);
  const { cases } = runtimeCases(fixtures);
  assert.deepEqual(fixtures, before);
  assert.equal(new Set(cases.map((item) => item.id)).size, cases.length);
  assert.equal(
    cases.filter((item) => item.operation === "runtime.environment").length,
    16,
  );
  const paths = new Set(
    cases.flatMap(
      (item) =>
        item.platform?.exchanges.map((exchange) => exchange.request.path) ?? [],
    ),
  );
  assert.deepEqual(
    paths,
    new Set([
      "/v1/transactions/mpp",
      "/v1/mpp/config",
      "/v1/transactions/x402-supported",
      "/v1/x402/config",
    ]),
  );
  for (const item of cases) {
    if (item.id.includes("seller-required"))
      assert.ok(item.input.product.endsWith("seller"));
    if (item.input.product === "x402-seller")
      assert.equal(item.input.tokens, undefined);
    if (item.id === "mpp-buyer.retry-policy")
      assert.equal(item.platform.exchanges.length, 1);
    if (item.id === "x402-buyer.token-rotation")
      assert.equal(item.expect.result.token_calls, 2);
  }
});

test("invalid or mismatched contract revisions are rejected", () => {
  assert.throws(() => checkContract(".", "main"), /full commit SHA/);
  assert.throws(
    () => checkContract(".", "0".repeat(40)),
    /clean contract checkout/,
  );
});
