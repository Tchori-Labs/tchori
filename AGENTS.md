# tchori — Agent Rulebook

This is the `tchori` engine repo — the product, not the company state root
(that's `Tchori-Labs/main`). Read this before opening a PR here.

## Identity

- All agent work is authored by the **Tchorizo** GitHub account.
- Always self-identify as an automated agent in any external interaction.

## Commits

- Conventional commits only: `feat:`, `fix:`, `test:`, `chore:`, `docs:`,
  `refactor:`. One logical change per commit.
- Never merge or approve your own PRs — CODEOWNERS review is the apply gate,
  same as `main`.

## Releases

- **No releases without board sign-off.** Tagging a version and letting
  goreleaser publish to GitHub Releases only happens after the board (a
  human) has explicitly approved it (recorded as a decision in `main`).
  Agents may prepare a release PR; they do not tag or push it themselves.

## Before opening a PR

Run, from the repo root:
```bash
gofmt -l .
go vet ./...
golangci-lint run
go test ./...
```
CI's `check` job (`.github/workflows/ci.yml`) re-runs the same four checks
and is a required status check — it must be green before merge.

## License

MPL-2.0. Files adapted from OpenTofu keep their original MPL header plus a
provenance comment naming the source file; original tchori files need no
header.

## Related

Parent company repo: `Tchori-Labs/main` (state root — company/, org/,
policies/, decisions/). This repo is the product itself.
