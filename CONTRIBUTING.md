# Contributing

Issues and pull requests are welcome. For anything beyond a small fix, consider
opening an issue first so we can agree on the approach before you write code —
and check [ROADMAP.md](ROADMAP.md) first; your idea may already be planned.

## Getting started

The module has no build system beyond the Go toolchain — clone it and go:

```bash
git clone https://github.com/jaracah/polymarket-us-go
cd polymarket-us-go
go test ./...
```

The test suite is fully hermetic: HTTP endpoints are exercised against
`httptest` servers and WebSocket streams against in-process echo servers, so
tests need no network access, no API credentials, and place no real orders. New
code should follow the same pattern — a test that dials `api.polymarket.us` will
not be accepted.

## Before sending a PR

```bash
gofmt -l .        # must print nothing
go vet ./...
staticcheck ./...  # go install honnef.co/go/tools/cmd/staticcheck@latest
go test -race ./...
```

CI runs the same checks.

## Ground rules

- **Keep the dependency footprint minimal.** The module has exactly one
  dependency (`coder/websocket`); adding another needs a strong reason.
- **Don't break the money invariants.** Order placement must stay single-attempt
  (the API has no client order id), rejections must stay errors, and fields that
  feed P&L (fill prices, position quantities) must fail loudly rather than
  degrade to zero. Tests pin these — keep them passing.
- **API changes:** exported types and signatures are the public contract.
  Additive changes are fine pre-v1; renames/removals should come with a clear
  upgrade note in the PR description.
- **Wire-format changes:** cite your source — the official polymarket-us SDKs
  (npm/PyPI) or observed API behavior — in the PR description.
- **Documentation:** exported identifiers get doc comments; if you change
  behavior described in the README or `doc.go`, update those too.

## Working with coding agents

Contributions written with the help of a coding agent (Claude Code, Cursor,
Copilot, etc.) are fine. [AGENTS.md](AGENTS.md) gives agents the project context
— the same checks and invariants above apply, and you are responsible for
reviewing what you submit.
