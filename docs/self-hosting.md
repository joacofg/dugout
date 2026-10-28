# Self-hosting the team server

How to bring up and run `dugout serve` for a team: one token per person, a repo allowlist, persistent state and TLS with Caddy. Alone on one machine you do not need it (see [the README](../README.md#try-it-alone-on-one-machine)).

`deploy/` has a Docker Compose setup; the commands below run from the repo root on a host with Docker. `dugout.example.internal` is a placeholder: use your host's name. Put the server on a host only your team can reach, such as a VPN or a tailnet.

## Bring it up

1. Copy the example `.env` and set the host name the laptops will use:

   ```sh
   cp deploy/.env.example deploy/.env
   # edit deploy/.env: DUGOUT_HOST=dugout.example.internal
   ```

   If the host is on a network that is not just the team's (a laptop on an office or a coffee shop wifi), also set `DUGOUT_BIND` to the IP the laptops reach it on, for example the Tailscale one (`tailscale ip -4`). Without it, Caddy listens on all interfaces. If Docker starts before that IP exists (when the machine boots), the Caddy container does not come up: repeat the `up -d` from step 4.

2. Put the certificate for `DUGOUT_HOST` in `deploy/certs/dugout.crt` and its key in `deploy/certs/dugout.key` (the folder is not committed and does not go into the image). The laptops validate it with their system's CAs, so they install nothing.

   - **Host on Tailscale:** `DUGOUT_HOST` is the host's name on the tailnet, `<machine>.<tailnet>.ts.net`. In the Tailscale console, under DNS, enable MagicDNS and HTTPS Certificates. Then, on the host:

     ```sh
     mkdir -p deploy/certs
     tailscale cert --cert-file deploy/certs/dugout.crt --key-file deploy/certs/dugout.key "$DUGOUT_HOST"
     chmod 600 deploy/certs/dugout.key
     ```

     It is a 90-day Let's Encrypt certificate: see [Renew the certificate](#renew-the-certificate). The host name ends up in the public Certificate Transparency logs.
   - **Your organization's certificate:** copy the certificate (with its chain) and the key to those two paths.

   Do not use `tls internal`: it would make every laptop trust Caddy's CA for any domain, with that CA's key stored on this host.

3. Create the state and save the admin token in your password manager. It is printed only once; if you lose it or it leaks, see [Rotate the admin token](#rotate-the-admin-token).

   ```sh
   docker compose -f deploy/docker-compose.yml run --rm dugout admin init -state /data/serve.json
   ```

4. Bring everything up:

   ```sh
   docker compose -f deploy/docker-compose.yml up -d
   ```

Without Docker, the same server is one command: `dugout serve -addr 0.0.0.0:7457 -allow-host dugout.example.internal -state serve.json`, behind a TLS proxy.

## Operate

The `dugout admin` commands run from your laptop and talk to the server over HTTPS. The admin token always goes through the environment, never through a flag:

```sh
export DUGOUT_SERVER=https://dugout.example.internal
read -rs DUGOUT_ADMIN_TOKEN && export DUGOUT_ADMIN_TOKEN   # paste the token: it does not stay in the history
```

### People and repos

Invite each person, or issue their token yourself, and load the repo allowlist. With no repos on the allowlist, the server lets none through.

```sh
dugout admin invite ana                     # prints a one-time join line to send ana
dugout admin token issue ana                # or the token itself, shown once
dugout admin token list
dugout admin token revoke ana               # also cancels a pending invite
dugout admin repo add github.com/acme/app
dugout admin repo add github.com/acme/payments ana beto   # only for ana and beto
dugout admin repo grant github.com/acme/payments carla
dugout admin repo ungrant github.com/acme/payments beto
dugout admin repo list
```

Send invites and tokens over a private channel. An invite works once and expires in 24 hours, or what `-expires` says, up to 7 days.

A repo with no members belongs to everyone who has a token; for a small team that trusts each other nothing more is needed. If not everyone has access to the repo, add it with its members (the token names): `serve` discards everyone else's work on that repo, like work on a repo outside the allowlist, and only the members see its workstreams, warnings, conflicts and bundles. A `grant` on a repo with no members leaves it for that person only. Running `repo add` again adds members; it never opens a repo up. `ungrant` does not remove the last member, because with no members everyone would see it: to hide it from everyone, `repo remove`. Revoking a token does not touch the members: reissue the token with the same name and the person sees their repos again.

**What leaves each laptop:** paths, branches and, when two people touch the same file, the in-flight version of the files they share, as a `git bundle`, with a secret scan before it is uploaded. `serve` hands it only to whoever overlaps with that workstream. That rule minimizes data, but it is not access control: among participants with a token, anyone can see another's in-flight code in an allowed repo with no members. Access control is the repo members.

### Team pause

It stops context injection and the held edits in every hook, without stopping the scans:

```sh
dugout admin pause
dugout admin resume
```

### Backup

The state (tokens, the allowlist with its members, the pause and which person each `daemon_id` is bound to) is a single file, `serve.json`, inside the `dugout-data` volume:

```sh
docker compose -f deploy/docker-compose.yml cp dugout:/data/serve.json ./serve.json.bak
```

`cp` works with the container running; the image has no shell, so `exec` cannot be used. Workstreams and warnings are not backed up: they live in memory and rebuild themselves when the daemons reconnect.

To restore, the file has to be owned by the container's user (65532) with mode 0600, or `serve` cannot read it and the container restarts in a loop:

```sh
chmod 600 serve.json.bak && sudo chown 65532:65532 serve.json.bak
docker compose -f deploy/docker-compose.yml cp -a serve.json.bak dugout:/data/serve.json
docker compose -f deploy/docker-compose.yml restart dugout
```

Do not edit `serve.json` by hand while `serve` is running: the next admin change, or the first request from a new daemon, overwrites it.

### Logs

```sh
docker compose -f deploy/docker-compose.yml logs -f dugout
```

- `stale epoch daemon=<id>`: that daemon's epoch went backwards, because its state directory was restored with the clock set back or the same `daemon_id` is on two machines. Restart `serve` (`docker compose -f deploy/docker-compose.yml restart dugout`), which forgets the epochs. If it is the same id on two machines, delete the state directory on one of them.
- `daemon_id of another person` (or a daemon that says `serve: this daemon_id belongs to another person`): that id is bound to another person, which happens when someone's token was reissued under another name or a laptop was migrated with its state directory. The binding is stored in `serve.json`, so restarting `serve` does not release it; otherwise anyone could take someone else's id while their daemon reconnects. Delete the `daemon_id` file in the state directory of the machine it does not belong to and restart its daemon, which starts with a new id. Revoking a person's token releases all their ids; reissue it afterwards if they stay.

If someone says a warning did not reach them, ask for their `dugout doctor --bundle` (see [common problems](install.md#common-problems)) and read it together with the `serve` log.

## The office view

`serve` also serves a live, read-only page of the office. Each person opens it with `dugout office`, which asks the server for a view token that only reads the office and dies with their own token, after 12 hours or when the server restarts. The page shows only the repos that person can see.

## Update

Update `serve` before the laptops; `dugout doctor` flags a version difference. On the server host, from the repo root:

```sh
git pull
docker compose -f deploy/docker-compose.yml build dugout
docker compose -f deploy/docker-compose.yml up -d dugout
```

Then each person updates their machine as in [Update](install.md#update). A daemon older than `serve` accepts gets a 426: it stops sending and tells its person to update.

## Metrics

```sh
dugout admin metrics   # with DUGOUT_SERVER and DUGOUT_ADMIN_TOKEN exported as in "Operate"
```

It prints the Prometheus text from `GET /v1/admin/metrics` (admin token only; it exists only with `-state`). No label carries people, tokens or code. The counters (`_total`) live in memory and start at zero every time `serve` restarts.

| Metric | What it says |
|---|---|
| `dugout_daemons_connected` | daemons with a warning stream open right now |
| `dugout_workstreams` | workstreams the daemons are seeing |
| `dugout_warnings` | open overlap warnings |
| `dugout_conflicts` | open reported conflicts |
| `dugout_messages_total{type}` | messages received on `/v1/msg`, by type: `full_state`, `conflicts` or `other` |
| `dugout_messages_refused_total{reason}` | messages rejected on `/v1/msg`: `too_old` is a dugout below the minimum protocol (426); if it grows, someone has to update |
| `dugout_bundles_relayed_total` | bundles delivered to another daemon |
| `dugout_stale_epochs_total` | messages ignored for coming from an old run of a daemon |
| `dugout_paused` | 1 if the team is paused |
| `dugout_persons` | people with an issued, non-revoked token |

`dugout admin metrics` also carries the team's agent metrics by repo: how many conflicts ended, and the share resolved before a commit or push.

## Rotate the admin token

There is no command to rotate it. If it was lost or leaked, rebuild the state:

```sh
docker compose -f deploy/docker-compose.yml rm -sf dugout
docker volume rm deploy_dugout-data
docker compose -f deploy/docker-compose.yml run --rm dugout admin init -state /data/serve.json
docker compose -f deploy/docker-compose.yml up -d dugout
```

Do not use `docker compose down -v`: it also deletes Caddy's volumes.

Afterwards, invite everyone again (or issue their tokens), and load the repo allowlist again.

## Renew the certificate

The Tailscale certificate expires after 90 days. Before that, on the host, repeat the `tailscale cert` from step 2 (it renews it when it is close to expiring) and restart Caddy, which reads the certificate on startup:

```sh
docker compose -f deploy/docker-compose.yml restart caddy
```
