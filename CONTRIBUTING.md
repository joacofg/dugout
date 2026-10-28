# Contributing to Dugout

Thanks for helping. Issues and pull requests are welcome; for anything larger than a fix, open an issue first so we can agree on the approach.

## Set up

You need Go 1.26+ (CI and releases use 1.27.x), git 2.50+ and, to try the plugin, Claude Code.

```sh
git clone https://github.com/joacofg/dugout.git && cd dugout
go test ./...
```

To try a change in Claude Code without installing anything:

```sh
go build -o plugin/bin/dugout ./cmd/dugout
plugin/bin/dugout daemon -server off &    # same-machine detection only
claude --plugin-dir plugin                # in each worktree of a test repo
```

`scripts/smoke-claude.sh` (hooks) and `scripts/smoke-mcp.sh` (MCP tools) drive a real Claude Code session against the plugin. They cost a few cents of API usage each, capped by `SMOKE_BUDGET_USD`.

[AGENTS.md](AGENTS.md) has the layout and the conventions; agents working on the repo read it too.

## Pull requests

- One change per PR, with tests. Keep `go test ./...` and `golangci-lint run ./...` (v2.15) green.
- Conventional commit titles with a scope: `feat(daemon): …`, `fix(serve): …`, `docs: …`.
- Everything in English: code, comments, messages and docs.
- Do not bump the version; releases do that.

## What belongs in this repo

This repo holds what users and contributors need: code, user docs and contributor docs. Design notes, research, plans, reviews, session handoffs and agent notes stay out, and so do design-tool artifacts such as `PRODUCT.md`, `DESIGN.md` or mockups. CI enforces it: the `public-tree` check fails on any tracked file outside the allowlist in `scripts/check-public-tree.sh`, and on a few names that must never appear. A new kind of file that users or contributors need goes on the allowlist in the same PR, with a reason.

## CI

Everything is in `.github/workflows/`. Actions are pinned to a commit, and Dependabot moves the pins.

| Workflow | When | What it checks |
|---|---|---|
| `ci` | push to main and every PR | `go vet`, `go test -race`, 1000 simulation seeds, cross build for macOS and Linux × amd64/arm64, golangci-lint, binary version = plugin version, plugin JSON and shell scripts parse, `claude plugin validate --strict`, actionlint, and the `public-tree` check |
| `ci-macos` | push to main and PRs that touch `cmd/`, hook, keychain, launchd, secrets, statedir or gitscan | the tests of those packages on macOS |
| `govulncheck` | push, PR and every Monday | reachable vulnerabilities in dependencies and in the Go toolchain |
| `docker` | PRs that touch `deploy/`, `.dockerignore` or `go.mod` | `deploy/Dockerfile` builds, the image starts, the compose file resolves. It publishes nothing |
| `release` | tag `vX.Y.Z` | the tag is on main and matches `install.go` and `plugin.json`; tests, govulncheck, binaries, `SHA256SUMS`, a build-provenance attestation and the GitHub release |

CI, release and the Dockerfile use Go 1.27.x even though `go.mod` allows 1.26: govulncheck flags standard-library vulnerabilities that only newer patch releases fix, and those binaries are the ones that get published. When bumping Go, change `go-version` in `ci`, `ci-macos`, `govulncheck` and `release`, and the Dockerfile image.

`.golangci.yml` is the standard set (errcheck, govet, ineffassign, staticcheck, unused) with the baseline from when it was added: errcheck ignores cleanup `Close`/`Remove`, deadlines, best-effort writes, and tests.

## Cutting a release (maintainers)

1. Bump the version in `cmd/dugout/install.go` (`var version`) and in `plugin/.claude-plugin/plugin.json`; `scripts/check-version.sh` fails the PR if they differ. Move the `Unreleased` section of `CHANGELOG.md` under the new version.
2. Merge to main, then `git tag vX.Y.Z && git push origin vX.Y.Z`.

Never move or delete a pushed tag: the Go module proxy keeps the first content it saw. Fix a bad release with the next patch version, and `retract` the bad one in `go.mod`.

## Security

Do not open a public issue for a vulnerability: see [SECURITY.md](SECURITY.md).

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
