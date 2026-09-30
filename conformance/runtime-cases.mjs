const clients = {
  "mpp-buyer": ["POST", "/v1/transactions/mpp"],
  "mpp-seller": ["GET", "/v1/mpp/config"],
  "x402-buyer": ["GET", "/v1/transactions/x402-supported"],
  "x402-seller": ["GET", "/v1/x402/config"],
};

export function runtimeCases(scenarios) {
  const cases = [];
  for (const [product, [method, path]] of Object.entries(clients)) {
    for (const [name, options, base] of [
      ["default", {}, "https://api.inflowpay.ai"],
      ["production", { environment: "production" }, "https://api.inflowpay.ai"],
      ["sandbox", { environment: "sandbox" }, "https://sandbox.inflowpay.ai"],
      [
        "override",
        { environment: "sandbox", base_url: "http://127.0.0.1:1234/prefix/" },
        "http://127.0.0.1:1234/prefix",
      ],
    ])
      cases.push({
        id: `${product}.environment.${name}`,
        suite: "runtime",
        operation: "runtime.environment",
        input: { product, ...options },
        expect: { result: { destinations: [`${method} ${base}${path}`] } },
      });

    const request = (headers) => ({
      method,
      path,
      headers,
      ...(method === "POST"
        ? {
            json: {
              challenge: {
                id: "test",
                realm: "seller.example",
                method: "inflow",
                intent: "charge",
                request: "e30",
              },
              options: {},
            },
          }
        : {}),
    });
    const add = (id, input, exchanges, response) => {
      const entry = response.json?.errors?.[0];
      cases.push({
        id: `${product}.${id}`,
        suite: "runtime",
        operation: "runtime.request",
        input: { product, ...input },
        platform: { exchanges },
        expect: {
          result: {
            code: entry?.code ?? "UNEXPECTED_ERROR",
            message: entry?.message ?? "request failed",
            http_status: response.status,
            endpoint: path,
            token_calls: input.tokens?.length ?? 0,
            request_id: response.headers?.["x-request-id"] ?? "",
            sensitive_headers: [],
          },
        },
      });
    };
    // Reuse shared rejection envelopes through the public endpoint each Go client exposes.
    // Approval lifecycle success/cancellation belongs to the payment suites, not raw HTTP here.
    for (const [id, scenario] of Object.entries(scenarios)) {
      if (!id.startsWith("auth.")) continue;
      const exchange = scenario.exchanges[0];
      if (exchange.response.status < 400) continue;
      const headers = exchange.request.headers;
      if (product === "x402-seller" && !headers["x-api-key"]) continue;
      if (id.startsWith("auth.seller-required") && !product.endsWith("seller"))
        continue;
      const input = headers.authorization
        ? { tokens: [headers.authorization.slice(7)] }
        : headers["x-api-key"]
          ? { api_key: headers["x-api-key"] }
          : {};
      add(
        id,
        input,
        [{ request: request(headers), response: exchange.response }],
        exchange.response,
      );
    }
    for (const status of [
      301, 302, 303, 307, 308, 400, 401, 403, 404, 409, 412, 500,
    ]) {
      const response = {
        status,
        headers: {
          location: "/must-not-follow",
          "x-request-id": "test-request",
          "set-cookie": "test-only-secret",
        },
      };
      add(
        `http.${status}`,
        { api_key: "test-only-key" },
        [{ request: request({ "x-api-key": "test-only-key" }), response }],
        response,
      );
    }
    const transient = { status: 503 };
    const unauthorized = { status: 401 };
    const headers = { "x-api-key": "test-only-key" };
    const exchanges = [{ request: request(headers), response: transient }];
    if (method === "GET")
      exchanges.push({ request: request(headers), response: unauthorized });
    add(
      "retry-policy",
      { api_key: "test-only-key" },
      exchanges,
      method === "GET" ? unauthorized : transient,
    );
    if (method === "GET" && product !== "x402-seller") {
      add(
        "token-rotation",
        { tokens: ["test-only-first", "test-only-second"] },
        [
          {
            request: request({ authorization: "Bearer test-only-first" }),
            response: transient,
          },
          {
            request: request({ authorization: "Bearer test-only-second" }),
            response: unauthorized,
          },
        ],
        unauthorized,
      );
    }
  }
  return { cases };
}
