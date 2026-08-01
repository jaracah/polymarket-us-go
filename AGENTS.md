# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

`polymarket-us-go` is an unofficial Go client library for the CFTC-regulated
**Polymarket US** exchange API (`api.polymarket.us`) — *not* the crypto
Polymarket CLOB API, which is a different protocol. The wire format follows
the official polymarket-us TypeScript and Python SDKs (v0.1.x).

It is a single flat package (`polymarket`) at the repo root with one
dependency, `coder/websocket`.

## Commands

```bash
go build ./...
go test -race ./...    # full suite; hermetic, fast, no credentials needed
gofmt -l .             # must print nothing
go vet ./...
staticcheck ./...      # go install honnef.co/go/tools/cmd/staticcheck@latest
```

Run all of the above before considering a change done — CI runs the same set.
Requires Go 1.23+.

## Layout

| File | Contents |
|---|---|
| `doc.go` | Package documentation — the design contract in prose |
| `client.go` | `Client`, public market data (markets, events, books, BBOs, settlements), HTTP core with retry logic |
| `trading.go` | Authenticated endpoints: orders, positions, balance |
| `auth.go` | `Signer` — Ed25519 request signing (`X-PM-*` headers) |
| `ws.go` | WebSocket streams: `MarketsStream` (books/BBOs/trades) and `PrivateStream` (executions/positions/balance) |
| `errors.go` | `APIError` — typed non-2xx responses |
| `example_test.go` | Runnable doc examples |

## Invariants — do not break these

These are deliberate design decisions, pinned by tests. Do not "fix" them.

1. **Order placement never retries.** The API has no client order ID, so a
   retried create would be a brand-new order. `CreateOrder` and
   `ClosePosition` make exactly one attempt. Reads and cancels are
   idempotent and retry 429s with linear backoff.
2. **Rejections are errors.** An order that comes back
   `EXECUTION_TYPE_REJECTED` returns an error with the exchange's reason —
   never a quiet zero-fill. A fill without a usable average price is also an
   error, not a 0¢ cost basis.
3. **Prices are integer cents.** Exchange decimals parse to `*C` int fields;
   the raw decimal strings (`Px`, `Qty`, `AvgPx`, …) are kept alongside so
   no precision is lost. Don't introduce floats into money math.
4. **Strict where money is counted, tolerant where it isn't.** Unparseable
   position quantities error (they feed reconciliation); malformed
   display-only fields degrade to zero values.
5. **Unauthenticated clients are strictly read-only.** A client from
   `NewClient` holds no credentials and must never touch a trading endpoint.
6. **One dependency.** Adding a dependency needs a strong reason and should
   be raised with the maintainer, not just done.

## Testing conventions

- Tests are hermetic: HTTP via `net/http/httptest`, WebSockets via
  in-process servers. Never write a test that touches the real API, requires
  credentials, or could place an order.
- New endpoint decoding gets a test with a realistic wire-format fixture;
  cite the source of the fixture (official SDK or observed behavior) in the
  PR or commit message.
- Keep the invariant-pinning tests passing; if one fails, the change is
  wrong, not the test.

## Style

- Standard Go style; `gofmt` and `staticcheck` clean.
- Exported identifiers get doc comments.
- Wire-format structs are unexported (`wireOrder`, `wirePosition`, …) and
  convert to exported types at the boundary — keep that separation.
- Public API is the contract: additive changes are fine pre-v1;
  renames/removals need an upgrade note.
- If behavior described in `README.md` or `doc.go` changes, update the prose
  in the same change.
