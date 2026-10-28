# Configuration

Dugout works with no configuration. This page covers what you can change: the repo file, the autonomy mode, duplicate detection, the server's tokens and TLS, and the agent metrics.

## `.dugout.yml`

Optional, at the repo root:

```yaml
repo_id: github.com/acme/app     # repo identity; defaults to the origin URL
target_branch: origin/develop    # defaults to origin/HEAD
escalate_after: 15m              # a conflict still standing this long after both agents were warned goes to both humans; 10m by default, off for never
ignore:
  add: ["docs/*.md"]             # added to the defaults (lockfiles, CHANGELOG*, *.snap, generated/, dist/)
  remove: ["*.snap"]
```

## Autonomy modes

When two agents clash, one keeps its change and the other yields. The mode, chosen per person and per repo, says how much the yielding agent settles without its human:

- **`auto`** (the default, like Claude Code's auto mode): it aligns its change or takes the clashing block out on its own, and tells you afterwards how. It escalates to both of you only when the two tasks contradict each other, when yielding would drop part of its task, or when aligning means deleting something or changing a shared contract (an API, a schema, a migration). You hear of a conflict, never of an overlap with no conflict.
- **`scoped`**: it aligns or yields within its task, and escalates only if yielding would drop part of it.
- **`supervised`**: it proposes how to yield and waits for your answer before changing its plan.

`dugout init` asks for it once; `dugout mode [auto|scoped|supervised]` shows or changes it, and so does `/dugout` inside Claude Code, mid-session. Every session starts by telling the agent the repo's mode; `/dugout` with no argument tells you. The choice stays on your machine, in the daemon's state directory.

## Who keeps a span

A conflict tells one side its lines stay and the other to align to them, the same on both sides. Agents that agree otherwise record it with `office_claim` or `office_yield`, and both read the result in their next warning and in `office_who`. When both claim the span, or both yield it, the default side keeps it.

## Escalation

Only what the agents cannot settle reaches the humans, and then both of them, by name, with what to decide. An agent escalates with the `office_escalate` MCP tool (what to decide and what each side wants, one question per session at a time). Dugout escalates on its own a conflict still standing `escalate_after` after both agents were warned, or one both branches claim. An agreed yield never escalates.

Each human gets a desktop notification (`osascript` on macOS) naming both sides, and each session on the conflict hears of it at its next hook. A notification the desktop cannot show is written to the terminal as OSC 9, and the agent is told to ask its human in the session instead. The names are each worktree's `git config user.name`, or the branch when it is unset.

## Duplicate detection

Off until you turn it on in the daemon's environment; `dugout doctor` and `dugout status` say whether it is on and why not:

```sh
DUGOUT_JUDGE=anthropic               # the only judge for now
ANTHROPIC_API_KEY=sk-ant-...         # read from the environment only: never stored, logged or shown
DUGOUT_JUDGE_MODEL=claude-haiku-5-5  # optional; the default
```

The daemon compares the declared intent of each of its workstreams with every other in-flight workstream of the repo, on its machine and, through the server, on the others, whether or not they share a file. Only the two intents go to the judge. A verdict is cached by the pair of intents, so a pair is judged again only when one of them changes. When two intents are the same task, both agents get it as context with the other's session, to agree on who does it; nothing is held.

The daemon under launchd or systemd does not see your shell's variables: set them with `launchctl setenv` (macOS, until you log out) or `systemctl --user set-environment` (Linux) and restart it with `dugout install -server <url>`, or run `dugout daemon` from a shell that has them. Each comparison costs about $0.00007 on the default model, the daemon makes at most 120 an hour, and `dugout status` shows what they cost.

## Tokens and TLS

- `dugout install`, `dugout login`, `dugout setup`, `dugout join` and the daemon refuse an `http://` server outside this machine, since the token and the bundles would cross the network in the clear. Use `https://`, or pass `-insecure` to accept the risk (`dugout install -insecure -server http://10.0.0.5:7457`); the daemon then logs a warning. `localhost` and other loopback addresses need no flag.
- `$DUGOUT_TOKEN` is sent only to the server in `$DUGOUT_SERVER` and to a server on this machine, never to another `-server`. For any other server, store its token with `dugout login -server URL`: the keychain keeps one token per server URL.
- A server on `127.0.0.1` is open to every user of that machine: on a shared machine, another user can start a listener on the same port before yours and receive the token. Run `dugout serve` on a host only your team can reach, with `https://`, and on a shared machine do not use the default loopback address.

## Pausing

`dugout pause` and `dugout resume` stop Dugout on one machine, and `dugout admin pause` stops it for the whole team. While paused, the hooks inject nothing and hold no edits, and the daemon keeps watching.

## Agent metrics

Dugout measures what the agents do about each conflict, with no rating or other human action: whether it was resolved before either side's commit or push, whether an edit Dugout held came back changed, how many messages the two sides' sessions exchanged and how many times it went to the humans. `dugout status` shows them for each conflict standing and for the latest that ended, with the totals since the daemon started. `dugout admin metrics` shows the team's, by repo, with the share of conflicts resolved before a commit or push. A pull, fast-forward or auto-fetch that advances the remote branch (or HEAD) while a conflict stands counts as committed, so that share is a lower bound.
