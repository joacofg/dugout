# Changelog

All notable changes to Dugout are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/); before 1.0, a minor version may break compatibility.

## [Unreleased]

The first public release.

### Added

- Overlap warnings: an agent learns at its next turn which other branches have its files in flight, with their declared intent.
- Conflict warnings from a speculative `git merge-tree` of every pair of workstreams that share a file, on one machine or across machines through the team server, with the file, the lines and the other side's version. The next edit of that file is held once.
- Who keeps a span: one side keeps its lines and the other aligns, the same on both sides; agents can settle it with `office_claim` and `office_yield`.
- Autonomy modes per person and repo (`auto`, `scoped`, `supervised`).
- Escalation to both humans, by the agents (`office_escalate`) or by a time rule, with desktop notifications.
- Opt-in duplicate-work detection by declared intent, with an LLM judge.
- The office: every session knows the other in-flight workstreams of its repo and their Claude Code session names; `office_who` lists them.
- Agent metrics per conflict, in `dugout status` and `dugout admin metrics`.
- `dugout serve`: one token per person, a repo allowlist with optional members, a team pause, Prometheus metrics and a live, read-only office view (`dugout office`).
- One-line install (`install.sh`, checked against `SHA256SUMS`), `dugout setup` and `setup --remove`, and team invites (`dugout admin invite`, `dugout join`).
- macOS (launchd, Keychain) and Linux (systemd, Secret Service or a 0600 file).

[Unreleased]: https://github.com/joacofg/dugout/commits/main
