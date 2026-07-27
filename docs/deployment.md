# Deployment requirements

What the operator's configuration management (Ansible, here) must provide. This repo
deliberately ships **no** deployment code — no roles, no playbooks, no unit files — because
the deployment belongs to whoever runs the server. What it does ship is an application that
can be configured entirely from one file, plus this contract describing what it expects.

The complete list of settings is [`almanack.conf.example`](../almanack.conf.example), which is
in systemd `EnvironmentFile` format so the same templated file works as `EnvironmentFile=` in
a unit or as `almanack --config <path>`.

## What the application provides

| Interface | Contract |
|---|---|
| `almanack serve` | Long-running process. Listens on `ALMANACK_LISTEN`. Exits non-zero on a configuration or migration failure, with the problems listed on stderr. |
| `almanack bootstrap --email <address> --name <name>` | Creates the first account and calendar on an empty database and prints an invite link. Signup is invite-only and there is no HTTP route to the first account, deliberately: the bootstrap window is never reachable from the internet. Refuses to run once any account exists. |
| `almanack backup <dir> [--prune]` | Takes a verified snapshot (`VACUUM INTO` → `PRAGMA integrity_check` on the output → fsync → atomic rename). **Exits non-zero if the snapshot is not intact**, which is the signal to alert on. `--prune` applies the generational retention from the config. |
| `almanack gen-vapid` | Prints a fresh VAPID keypair. Run once, ever, at first deployment. |
| `almanack seed` | Creates a demo family. Development only. |
| `almanack version` | Prints the build version. |
| `GET /healthz` | No auth. `200` with `{"status":"ok",...}`, or `503` when degraded. Reports database reachability, scheduler heartbeat age, last backup age and result, consecutive SMTP failures, per-push-service error counts, and disk usage. |
| systemd readiness | If `NOTIFY_SOCKET` is set, the process sends `READY=1` **after** migrations complete, so a health check can distinguish "still migrating" from "dead". Use `Type=notify`. |
| systemd watchdog | If `WATCHDOG_USEC` is set, the scheduler loop pings the watchdog. Set `WatchdogSec` so that a hung scheduler is restarted rather than silently ceasing to send reminders. |
| Signals | `SIGTERM`/`SIGINT` drain in-flight requests and stop cleanly. |
| Logs | Structured, to stdout only. Nothing to rotate. |

## What the operator must provide

**Service account and paths.** An unprivileged user; a data directory it owns (the SQLite
file plus `-wal`/`-shm` siblings and the backup directory live there); the config file readable
by that user but not world-readable, since it contains the VAPID private key.

**Sandboxing.** The service needs write access only to its data directory, and outbound network
access to the push services and the local MTA. Any hardening that grants that is fine; do not
block outbound HTTPS, and do not make the data directory read-only.

**Clock discipline.** Order the service after time synchronisation. A server that boots with a
dead CMOS battery and a 1970 clock would otherwise either flush every pending reminder at once
or mark them delivered without sending them. The application refuses to start its scheduler if
the clock predates its own build date, but that is a backstop, not a substitute.

**TLS reverse proxy.** The app listens on localhost and speaks plain HTTP; the proxy terminates
TLS and forwards. Requirements: pass `X-Forwarded-For` (and list the proxy in
`ALMANACK_TRUSTED_PROXIES`); do not add a `Content-Security-Policy` header, as the application
sets its own and a second one is intersected, which will break the UI; allow request bodies of
at least 2 MB for avatar uploads; use a read timeout above 30 s. HTTPS is not optional — PWA
installation and Web Push both refuse an insecure origin, and the app rejects a non-https
`ALMANACK_BASE_URL` at startup.

**Certificates.** Automated renewal, and an alert when fewer than ~14 days remain. Certificate
lifetimes are shrinking to 47 days by 2029, which leaves no slack for renewal that has quietly
been broken for a month.

**A local MTA.** Listening on `ALMANACK_SMTP` (`127.0.0.1:25` by default), configured to relay to
whatever mailbox provider the family uses. The application deliberately has no SMTP credentials
of its own. Without working mail there is no password reset, no email reminders, and no failure
alerting.

**Backups on a timer.** Run `almanack backup <dir> --prune` hourly, as the service user. Sync the
directory off-host at least daily, ordered after the snapshot, copying only `almanack-*.db` and
never `*.tmp`. Alert on non-zero exit — that is a failed integrity check, i.e. the database is
damaged and the clock is running on how long the good generations survive.

**Failure alerting.** Wire a mail-on-failure hook to the service unit and to both timers. This
is the single most important thing in the deployment: everything else in this system fails
silently on a machine with nobody watching it.

**Off-host disaster-recovery set.** Snapshots alone will not rebuild this server. Also keep, off
the machine: the config file, release binaries for amd64 **and** arm64, the Go toolchain tarball
and vendored source, the deployment repository itself, the secret-store password, registrar and
DNS credentials, the certificate directory, and a note of the router port forwards.

**Upgrades and rollback.** Deploy a new binary and restart; migrations run at startup, taking
their own pre-migration snapshot first. Migrations are expand/contract — each release stays
compatible with the previous binary for one version — so **rollback is putting the old binary
back**, with no restore and no data loss. The binary refuses to start against a schema newer
than it knows, so a mistaken downgrade fails loudly instead of corrupting data. Restoring a
pre-migration snapshot is the catastrophe path only, and it discards everything the family
entered after the upgrade.

## First deployment, in order

1. Generate the VAPID keypair (`almanack gen-vapid`) and put it in the secret store. Never rotate it.
2. Template the config file from `almanack.conf.example`.
3. Install the binary, create the user and data directory, install the unit and timers.
4. Configure the MTA and send yourself a test message.
5. Point DNS at the host, forward the ports, issue the certificate.
6. Start the service; confirm `/healthz` returns 200 and that the readiness signal arrived.
7. Run `almanack bootstrap --email you@example.org --name "Your name"` on the host. Sign in with the
   printed password, change it immediately, and send the printed invite link to the rest of the
   family. There is no way to create the first account over HTTP — that is the point.
8. Check that the ops heartbeat mail arrives the next morning. If it does not, the monitoring
   is not working, and everything after this point would fail silently.
9. Rehearse a restore on a scratch machine before the family starts relying on it — and again
   once a year, because a restore procedure that was verified in 2026 proves nothing in 2033.
