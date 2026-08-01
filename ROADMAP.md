# Roadmap

Status at a glance: the library is **feature-complete for v0.1** and the test
suite is green, but it has not yet accumulated live production mileage. Treat
it as beta until the soak milestone below is done.

## In progress — live soak testing

The test suite is fully hermetic (fixtures derived from the official
polymarket-us SDKs), which proves the client against the *documented* wire
format but not against the live exchange's edge cases. Before recommending
unattended real-money use, the client is being soak-tested against production:

- [ ] Multi-day run with 1-lot orders: resting orders, cancels, fills,
      position closes
- [ ] Raw WebSocket frame capture, checked for frames the client
      doesn't recognize
- [ ] Three-way reconciliation after every session: local execution log vs.
      `Positions()` vs. `Balance()` — must agree to the cent
- [ ] Failure rehearsal: network loss mid-session, connection drop between
      order send and response (the ambiguous-failure path)

## Next

- [ ] `examples/` supervised-bot skeleton: reconnect-with-resubscribe loop,
      heartbeat staleness watchdog, reconcile-before-resend recipe for
      ambiguous `CreateOrder` failures
- [ ] README operational guide covering the same recovery recipes in prose
- [ ] Opt-in (manually triggered) live smoke workflow hitting public
      market-data endpoints only — a canary for upstream wire changes
- [ ] Tag `v0.1.0` once the soak milestone is clean

## Later

- Clock-skew tolerance on request signing
- Jittered retry backoff
- Possible `session` package graduating the examples skeleton into supported
  code, once its design has survived production use
- Track upstream API changes toward its v1 (the exchange API is young; wire
  format follows the official TypeScript/Python SDKs)

Suggestions and reports from anyone running the client against production are
especially welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).
