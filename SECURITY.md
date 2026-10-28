# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, with **Report a vulnerability** under this repository's [Security tab](https://github.com/joacofg/dugout/security/advisories/new). Do not open a public issue or pull request for it.

Include what you found, how to reproduce it, and the version (`dugout version`). You should hear back within a week. Once a fix is released, the advisory is published with credit to you, unless you prefer otherwise.

## Supported versions

Dugout is pre-1.0: only the latest release gets security fixes.

## Scope

Dugout runs a daemon on each developer's machine and, for a team, one server. These are in scope:

- **Tokens and invite codes:** a person's token, the admin token or an invite code leaking to anyone but its server, through logs, hook output, MCP results, `dugout status`, diagnostic bundles (`dugout doctor --bundle`) or the network. A token sent over plain `http://` to a host outside the machine without `-insecure` is a bug.
- **Code leaving the machine:** a git bundle carrying a file outside the files both sides have in flight, a file on the credentials denylist (`.env*`, keys, certificates and the like), or a secret the scan should have caught.
- **Access control on the server:** a participant seeing workstreams, warnings, conflicts, bundles or the office view of a repo they are not a member of, or a view token doing more than reading the office.
- **Prompt injection:** text from a teammate (paths, branches, intents, conflict lines) reaching an agent outside the sanitized `<dugout-context>` data block, or breaking out of it.
- **The local machine:** the plugin running a `dugout` binary that a repo ships, or the daemon writing to a user's repo or git index.

Out of scope: what participants with a valid token can see by design (the paths, intents and shared in-flight code of the repos they can access; see "What it sends, and to whom" in the [README](README.md)), and a server on the default loopback address of a machine shared with untrusted users, which the README warns against.
