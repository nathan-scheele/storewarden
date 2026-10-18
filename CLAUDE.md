# storewarden

Design: `docs/design.md`. Language: `GLOSSARY.md`. Decisions: `docs/adr/`.

## Working here

- **Check**: `make check` (gofmt, vet, the never-public guard, staticcheck, `test -race`). `make ci` adds govulncheck on go.mod's Go version; it is what `.github/workflows/ci.yml` runs. Every build needs `-tags ts_omit_peerapiserver`; the Makefile carries it.
- **Dependencies**: SDK and tsnet behaviour the code relies on is in `docs/agents/dependency-notes.md`. Read it before touching `internal/authz`, the MCP handler, or tsnet setup.
- **Fixtures**: record Shopify responses with `scripts/record-fixture.sh NAME`, from `cmd/storewarden/testdata/NAME.graphql`. Every fixture comes from the dev store this way.
- **Shopify**: only the dev store in `.env`, through `internal/shopify` or `scripts/record-fixture.sh`.
- **Tailnet testing**: only through `scripts/smoke-tailnet.sh`, in a tailnet set aside for it.

## Agent skills

### Issue tracker

GitHub Issues on nathan-scheele/storewarden, via `gh`. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five: needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: root `GLOSSARY.md` + `docs/adr/`. See `docs/agents/domain.md`.
