// Package polymarket is an unofficial Go client for the Polymarket US
// exchange API (api.polymarket.us). It is not affiliated with Polymarket.
//
// Without a Signer the client is strictly read-only public market data
// (NewClient — no credentials, nothing is ever sent to trading endpoints).
// With one (NewAuthedClient) the trading endpoints and the WebSocket
// streams become available:
//
//	c := polymarket.NewClient(nil)
//	bbo, err := c.FetchBBO(ctx, "some-market-slug")
//
//	signer, err := polymarket.NewSigner(keyID, secretKey)
//	c = polymarket.NewAuthedClient(nil, signer, "", "")
//	res, err := c.CreateOrder(ctx, polymarket.Order{...})
//
// The wire protocol follows the official polymarket-us TypeScript and
// Python SDKs (v0.1.x): requests are authenticated with Ed25519-signed
// X-PM-* headers, public market data is served from a gateway host, and
// trading plus both WebSocket streams use the api host.
//
// # Prices and quantities
//
// Prices cross the wire as decimal-dollar Amount objects ({"value":"0.55",
// "currency":"USD"}). For convenience this package parses prices to
// integer cents (and quantities to float64) wherever it decodes a
// response; fields set by the exchange also carry the raw decimal strings
// (Px, Qty, AvgPx, ...) so no precision is ever lost to the conversion.
//
// # Errors
//
// A non-2xx response surfaces as *APIError, carrying the HTTP status and
// the exchange's reason; match it with errors.As. Order rejections arrive
// inside 2xx execution streams and surface as plain errors from
// CreateOrder/ClosePosition with the reject reason.
//
// # Retries
//
// Reads and cancels retry 429s with linear backoff. Order placement makes
// exactly one attempt: the API has no client order id, so a retried create
// would be a brand-new order. After an ambiguous failure, reconcile with
// OpenOrders/Positions before re-sending.
package polymarket
