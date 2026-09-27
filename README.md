# polymarket-us-go

[![Go Reference](https://pkg.go.dev/badge/github.com/jaracah/polymarket-us-go.svg)](https://pkg.go.dev/github.com/jaracah/polymarket-us-go)
[![CI](https://github.com/jaracah/polymarket-us-go/actions/workflows/ci.yml/badge.svg)](https://github.com/jaracah/polymarket-us-go/actions/workflows/ci.yml)

Unofficial Go client library for the [Polymarket US](https://polymarket.us)
exchange API. Not affiliated with Polymarket.

This targets the CFTC-regulated **Polymarket US** exchange (`api.polymarket.us`)
— _not_ the crypto Polymarket CLOB API, which is a completely different
protocol. The wire format follows the official
[TypeScript](https://www.npmjs.com/package/polymarket-us) and
[Python](https://pypi.org/project/polymarket-us/) SDKs (v0.1.x). Since the
upstream API is young, expect this client to track breaking changes until it
stabilizes.

## Status

**Beta — live testing in progress.** The API surface is complete and the tests
are green, but the client is still accumulating live production usage. See
[ROADMAP.md](ROADMAP.md) for what's being verified and what's planned.

## Install

```bash
go get github.com/jaracah/polymarket-us-go
```

Requires Go 1.23+. One dependency:
[`coder/websocket`](https://github.com/coder/websocket) (itself
dependency-free).

Full API documentation is on
[pkg.go.dev](https://pkg.go.dev/github.com/jaracah/polymarket-us-go).

## Public market data (no credentials)

```go
import polymarket "github.com/jaracah/polymarket-us-go"

c := polymarket.NewClient(nil)

markets, err := c.ListMarkets(ctx, polymarket.MarketFilter{ActiveOnly: true, Limit: 20})
market, err := c.FetchMarket(ctx, "btc-100k")
event, err := c.FetchEvent(ctx, "super-bowl-2027")

book, err := c.FetchBook(ctx, "btc-100k") // full depth
bbo, err := c.FetchBBO(ctx, "btc-100k")   // top of book
fmt.Println(bbo.BidC, bbo.AskC)            // integer cents
fmt.Println(bbo.BidPx, bbo.AskPx)          // raw decimal strings

settle, err := c.FetchSettlement(ctx, "btc-100k") // outcome of a resolved market
```

A client built with `NewClient` is strictly read-only: it holds no credentials
and never touches a trading endpoint.

## Trading (authenticated)

Generate an API key at
[polymarket.us/developer](https://polymarket.us/developer). You get a key ID
(UUID) and a base64-encoded Ed25519 secret key; requests are signed with
`X-PM-*` headers automatically.

```go
signer, err := polymarket.NewSigner(os.Getenv("POLYMARKET_KEY_ID"), os.Getenv("POLYMARKET_SECRET_KEY"))
c := polymarket.NewAuthedClient(nil, signer, "", "") // "" = production hosts

// Cheap connectivity/credentials probe
bal, err := c.Balance(ctx)

// Place a limit order: buy 100 LONG contracts at 55¢, immediate-or-cancel
res, err := c.CreateOrder(ctx, polymarket.Order{
    MarketSlug: "btc-100k",
    Intent:     polymarket.IntentBuyLong,
    Quantity:   100,
    PriceC:     55,
})
fmt.Println(res.FillCount, res.AvgPriceC, res.State)

// Resting orders
res, err = c.CreateOrder(ctx, polymarket.Order{
    MarketSlug:  "btc-100k",
    Intent:      polymarket.IntentBuyLong,
    Quantity:    100,
    PriceC:      52,
    TimeInForce: polymarket.TIFGoodTillCancel,
    PostOnly:    true, // reject instead of crossing
})

open, err := c.OpenOrders(ctx)                       // optionally filter by slugs
err = c.CancelOrder(ctx, res.OrderID, "btc-100k")
ids, err := c.CancelAllOrders(ctx)                   // flatten-fast path
res, err = c.ClosePosition(ctx, "btc-100k")          // market-close one position
positions, err := c.Positions(ctx)                   // map[slug]Position, cursor-paginated
```

Market data and trading are served from different hosts, so their connection
pools warm independently: polling `Balance` keeps the trading host's TLS
connection hot (the one order latency depends on), while any cheap read like
`FetchBBO` does the same for the market-data gateway. A bot that only polls
market data still pays connection setup on its first order.

The exchange trades LONG and SHORT as separate instruments, so there are four
order intents (`IntentBuyLong`, `IntentSellLong`, `IntentBuyShort`,
`IntentSellShort`) and prices always quote the named instrument — no `100 − p`
mental arithmetic.

## WebSocket streams

Both streams require credentials (the exchange authenticates even market data).
The API is pull-based: dial, subscribe, then read messages in a loop.

```go
// Market data: books, BBOs, trades
ms, err := c.DialMarkets(ctx)
defer ms.Close()
ms.SubscribeBooks(ctx, "books-1", "btc-100k")
ms.SubscribeTrades(ctx, "tape-1", "btc-100k")
for {
    msg, err := ms.Next(ctx)
    if err != nil {
        break // transport error: redial and re-subscribe
    }
    switch {
    case msg.Book != nil:
        // full-depth refresh
    case msg.Trade != nil:
        // one print off the tape
    case msg.Err != "":
        // server rejected the subscription keyed by msg.RequestID
    }
}

// Private data: order executions, positions, balances
ps, err := c.DialPrivate(ctx)
defer ps.Close()
ps.SubscribeOrders(ctx, "orders-1")
ps.SubscribePositions(ctx, "pos-1")
ps.SubscribeBalance(ctx, "bal-1")
for {
    msg, err := ps.Next(ctx)
    if err != nil {
        break
    }
    if msg.Execution != nil {
        // fills, cancels, rejections as they happen
    }
}
```

Subscriptions do not survive a reconnect — after a transport error, redial and
re-subscribe.

Both streams expose `Ping(ctx)`, a protocol-level ping that waits for the
pong. Heartbeat from a separate goroutine while the main loop sits in `Next`
— it keeps idle connections alive through load-balancer timeouts and detects
a dead transport early:

```go
go func() {
    t := time.NewTicker(30 * time.Second)
    defer t.Stop()
    for range t.C {
        if err := ms.Ping(ctx); err != nil {
            return // transport dead: redial and re-subscribe
        }
    }
}()
```

## Design notes

- **Prices parse to integer cents** (`PriceC`, `AvgPriceC`, …) because binary
  contracts trade in 1–99¢ and integer math avoids float drift in P&L. Every
  exchange-set number also carries the raw decimal string (`Px`, `Qty`, `AvgPx`,
  …), so nothing is lost if the exchange ever quotes sub-cent ticks.
- **Order placement never retries.** The API has no client order ID, so a
  retried create would be a brand-new order. `CreateOrder` and `ClosePosition`
  make exactly one attempt; after an ambiguous failure, reconcile with
  `OpenOrders`/`Positions` before re-sending. Reads and cancels are idempotent
  and retry 429s with backoff.
- **Typed HTTP errors.** Non-2xx responses surface as `*polymarket.APIError`
  carrying the status code and the exchange's reason — match with `errors.As` to
  branch on 401/404/429.
- **Rejections are errors.** An order that comes back `EXECUTION_TYPE_REJECTED`
  returns an error carrying the exchange's reason, never a quiet zero-fill. A
  fill reported without a usable average price is also an error rather than a 0¢
  cost basis.
- **Strict where money is counted, tolerant where it isn't.** Unparseable
  position quantities error (they feed reconciliation); malformed display fields
  degrade to zero values.

## Contributing

Issues and pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md)
for the checks to run and the invariants to preserve. The test suite is fully
hermetic (`go test -race ./...` needs no network access or credentials).

## Disclaimer

This is an unofficial client, not affiliated with or endorsed by Polymarket.
Trading involves risk of loss; use at your own risk, and test against small
orders before automating anything with real money.

## License

[MIT](LICENSE)
