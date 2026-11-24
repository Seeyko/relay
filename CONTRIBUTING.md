# Contributing to Relay

Thanks for helping! Issues and pull requests are welcome.

## Ground rules

- Keep Relay small: one binary, SQLite, no UI, no external services required to run.
- Official network APIs first. Browser automation lives outside the server, behind the agent task queue.
- Never commit credentials, tokens or real account data. Tests must run offline against fake servers.

## Development

```bash
go vet ./...
go test ./...
```

Go version: see `go.mod`.

## Adding a connector

1. Create a package under `internal/providers/<name>/`.
2. Implement `providers.Client` (`Identify`, `Publish`, `Metrics`, `Comments`, `Reply`, `History`).
   Return `providers.ErrUnsupported` for what the network does not offer, and `providers.Invalid(...)` for
   content the network will never accept (so the scheduler does not retry it).
3. Register it in an `init()` with `providers.Register`, listing its `CredentialFields`.
4. Accept a `base_url` credential override so tests can point it at an `httptest` server.
5. Import the package in `cmd/relay/main.go` and add a test in `internal/api/api_test.go`.
6. Document it in the README connectors table.

## Pull requests

- One topic per PR, with tests.
- Describe what you verified against the real network, if anything.
- `main` is protected: changes land through a pull request once the `ci / test` check is green.
  Force pushes and deleting `main` are blocked. PRs are squash-merged.

The protection lives in [`.github/rulesets/main.json`](.github/rulesets/main.json) and is imported in
**Settings → Rules → Rulesets → New ruleset → Import a ruleset**.
