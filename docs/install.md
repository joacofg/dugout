# Install and use Dugout

How to install Dugout on your machine, what it does while you work, and how to update, troubleshoot and remove it. To run the team server, see [self-hosting](self-hosting.md).

## Requirements

- macOS, or Linux with systemd.
- [Claude Code](https://claude.com/claude-code).
- A git whose `merge-tree --write-tree --merge-base` accepts trees (tested with 2.50.1). With an older git the daemon logs `stage 2 disabled` and reports only overlaps, never conflicts.
- For a team: the URL of your team's server and an invite or a token from its admin. Alone on one machine you need neither.

## Install

One line downloads the release binary for your machine, checks it against the release's `SHA256SUMS`, puts it in `~/.local/bin` (`DUGOUT_BIN_DIR` changes it) and runs `dugout setup`:

```sh
curl -fsSL https://raw.githubusercontent.com/joacofg/dugout/main/install.sh | sh
```

`DUGOUT_VERSION` picks a release instead of the latest. Each release archive also carries a build-provenance attestation: `gh attestation verify dugout_<version>_<os>_<arch>.tar.gz -R joacofg/dugout` checks that GitHub Actions built it from this repo. With Go 1.26+ you can build it instead, then run `dugout setup` yourself:

```sh
go install github.com/joacofg/dugout/cmd/dugout@latest
dugout setup
```

If `dugout` does not show up, add `~/.local/bin` (or `$(go env GOPATH)/bin`) to your `PATH`.

`dugout setup`:

1. **Installs the daemon** as a service that starts with your session (a LaunchAgent on macOS, a `systemd --user` unit on Linux), against `-server URL`; without it, the server of the daemon already installed, else `$DUGOUT_SERVER`, else `off`: this machine's sessions only. For a team server with no token stored yet, it asks for one as `dugout login` does.
2. **Adds the Claude Code plugin** with the `claude plugin` CLI, from the `joacofg/dugout` marketplace. Restart Claude Code, or run `/reload-plugins` in it, to load it. Without an SSH key for GitHub, set `CLAUDE_CODE_PLUGIN_PREFER_HTTPS=1` first.
3. **Runs `dugout init`** in the repo you are in: it tracks the repo and asks, once, how much autonomy your agents get there (`-mode` answers for it; see [autonomy modes](configuration.md#autonomy-modes)). Other repos are tracked the first time a session opens in them, in the default mode, or with `dugout init` in each.
4. **Ends with `dugout doctor`**, which lists what is missing. Every line with `✗` says how to fix it.

Every file setup edits outside Dugout's own state directory (the service, the binary's link, Claude Code's `settings.json`, `plugins/known_marketplaces.json` and `plugins/installed_plugins.json`) is backed up first, and `dugout setup --remove` puts it back (see [Uninstall](#uninstall)).

### Joining a team

The admin runs `dugout admin invite <you>` and sends you the line it prints over a private channel:

```
Join our Dugout: dugout join 'https://dugout.example.internal#dgi_…'
```

Paste it into a terminal or give it to your coding agent. `dugout join` redeems the code for your token, stores it as `dugout login` does and runs `dugout setup` against that server. Without Dugout installed yet, use the installer form the invite also prints: `curl -fsSL …/install.sh | sh -s -- join '…'`. A code is for one person, works once and expires in 24 hours, or what the admin's `-expires` says, up to 7 days. Neither command prints the token or the code, and no hook, MCP result, `dugout status` or SessionStart carries them.

If the server's URL ends in `.ts.net`, it is on the admin's tailnet: install Tailscale, join with the invitation they sent you, and check that you can reach it (`curl -sI <url>` answers, even with a 401). Its certificate is from Let's Encrypt, so you install no CA.

### By hand

The same steps one by one:

```sh
dugout login -server https://dugout.example.internal     # reads the token without echo
dugout install -server https://dugout.example.internal   # the daemon, as a service
```

Then, in Claude Code:

```
/plugin marketplace add joacofg/dugout
/plugin install dugout@dugout
```

and `dugout init` in each repo.

### Where the token goes

On macOS the token goes to the Keychain. On Linux it goes to your desktop keyring through `secret-tool` (`libsecret-tools` on Debian and Ubuntu) or, without it, to a 0600 `tokens.json` in the state directory, unencrypted: your user and root can read it, and it travels with backups of your home. `dugout login` says where it ended up; after installing `secret-tool`, run it again.

## Use

Work as usual, one worktree per task. Dugout only makes itself noticed when it needs to:

- **When a session starts, and when you send a prompt,** the agent receives a summary of who else is touching your files.
- **When your change and someone else's really conflict** on the same lines, the agent receives the conflict with the other side's lines, and its next edit of that file is held once so it reads the warning before it writes. Depending on the [autonomy mode](configuration.md#autonomy-modes), it aligns, yields or asks you.
- **The intent:** at the start, Dugout asks the agent to declare in one sentence what it is going to do (the `office_declare_intent` tool). Your prompt is never sent. Everyone with a token for the repo can see that sentence and your files in flight, and it reaches the agents of whoever touches the same files.
- **Every new warning** shows you one line, for example `Dugout: api.go conflicts with branch "ana/login"`.
- **`dugout status`** shows your workstreams, their intent, the conflicts and overlaps, the pause, the agent metrics and whether there is a connection to the server.
- **`dugout office`** opens the read-only office view of your team's server in the browser.
- **`dugout pause` and `dugout resume`** stop Dugout on your machine: the hooks inject nothing and hold no edits, and the daemon keeps watching. The admin can also pause the whole team.

### What leaves your machine

- The repo, the branch and the paths of the files you changed.
- The intent your agent declares (up to 280 characters).
- When someone else touches the same file, your in-flight version of the files you both touch, as a `git bundle`; the rest of your change does not leave.
  - Files that are credentials by name never go into the bundle, regardless of case: `.env*`, `*.pem`, `*.key`, `id_*`, `.npmrc`, `.netrc`, `.pgpass`, `.git-credentials`, `.pypirc`, `credentials.json`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `*.ppk` and `*.tfstate*`. Neither do files larger than 1 MB.
  - Before it is uploaded it is scanned for secrets (private keys, PGP included; AWS, GitHub, GitLab, npm, Slack and Slack webhook, Google, Stripe, Anthropic and OpenAI tokens; JWTs, and URLs with a password, except those for `localhost` or example domains). If it finds one, nothing is uploaded.
- With the [duplicate judge](configuration.md#duplicate-detection) on, to the Claude API: your agent's intent and the other workstream's, and nothing else.

The other participants can see your in-flight code in the repos they share with you: that is the point of the product.

## Update

Run the installer again, or `go install github.com/joacofg/dugout/cmd/dugout@latest`, then:

```sh
dugout setup     # restarts the daemon with the new binary and updates the link the hooks run
claude plugin marketplace update dugout
claude plugin update dugout@dugout
```

Restart Claude Code afterwards: a new plugin version only loads when you open it again. The plugin's hooks run the binary `dugout install` linked, never one found on the `PATH`; your shell's `dugout` still comes from the `PATH`, so if `dugout version` did not change, check your `PATH`.

The team's server is updated before the laptops. If your dugout is older than the server, or the other way round, Dugout warns you once per session and `dugout doctor` shows both protocols. What works with each combination (daemon in the row, server in the column):

| daemon \ serve | 1 | 2 |
|---|---|---|
| 1 | everything works | everything works |
| 2 | no intents from others; doctor asks to update the server | everything works |

## Common problems

| Symptom | What to do |
|---|---|
| `dugout doctor` says `no token for <url>` | `dugout login -server <url>`, then `dugout install -server <url>` to restart the daemon |
| `the server rejected your token` | ask the admin for a new token or invite and log in again |
| `the LaunchAgent points to a binary that no longer exists` | you moved or deleted the binary: `dugout install -server <url>` again |
| the daemon log says `serve: this daemon_id belongs to another person` | that id is bound to someone else on the server. Delete the `daemon_id` file in the state directory and restart the daemon (`dugout install -server <url>`), which starts with a new id. If that fails, ask the admin: revoking the token of the person who holds it releases it |
| `dugout doctor` says `duplicate detection is off: DUGOUT_JUDGE is anthropic but ANTHROPIC_API_KEY is not set` | the daemon does not see the key: see [duplicate detection](configuration.md#duplicate-detection) |
| the agent sees nothing from Dugout | `dugout status` and `dugout doctor`; if the plugin is there but the daemon does not answer, read the daemon log (`~/Library/Logs/dugout/daemon.log` on macOS, `daemon.log` in the state directory on Linux) |
| something is wrong and you do not know what | `dugout doctor --bundle` writes `dugout-diag-<date>-<time>.tar.gz` in your home folder (`-o <path>` picks another): send it to the admin over a private channel. It carries no tokens or code: the doctor output, the versions, the service file, the last 500 lines of the daemon log and a few small files from the state directory |

## Uninstall

```sh
dugout setup --remove
```

It takes out the plugin and the marketplace if setup added them, through the same `claude plugin` CLI (so Claude Code's own later edits stay), uninstalls the daemon and puts each file setup changed back byte for byte. A file that changed after setup is kept, and remove says where its original is. Dugout's own state and logs stay.

Without setup, `dugout uninstall` removes the daemon's service and `/plugin uninstall dugout@dugout` the plugin. The token stays in the keychain (service `dugout`) or in `tokens.json`; on Linux, `secret-tool clear service dugout account <url>` deletes it.

## Linux

The same steps work on Linux with systemd. What changes:

- **The daemon** runs as a `systemd --user` unit in `~/.config/systemd/user/dugout.service` (or under `$XDG_CONFIG_HOME`): it starts with your session and systemd restarts it every 10 s if it dies. `dugout install -server <url> -dry-run` shows the unit without installing anything, and `systemctl --user status dugout` tells you whether it is running. By default it stops when you close your last session; on a machine you reach over ssh, `loginctl enable-linger` keeps it running.
- **The state and the log** are in `~/.local/state/dugout` (or `$XDG_STATE_HOME/dugout`); the daemon log is `daemon.log` there. The socket is at `$XDG_RUNTIME_DIR/dugout.sock`.
- **If `dugout install` says `Failed to connect to bus`:** `systemctl --user` needs your user's systemd session. Log in normally (ssh or the console, not `su`) and check that `XDG_RUNTIME_DIR` is set.
- **If `dugout doctor` says the daemon does not answer but `systemctl --user status dugout` shows it running:** the hooks look for the socket in the `XDG_RUNTIME_DIR` of the session you opened Claude Code from. Open it from a session that has it, like the one you ran `dugout install` in.
- `dugout doctor --bundle` carries the unit (`dugout.service`) instead of the LaunchAgent.
