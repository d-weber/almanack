# Installing Almanack

A worked example, start to finish, of getting Almanack running — first on the machine in
front of you, then on a server your family can actually use.

This is **an example to copy and adapt**, not a deployment system. Nothing here is run for
you. [docs/deployment.md](deployment.md) is the contract this example satisfies: read it
when you want to know *why* a step is here, or when your setup differs from this one.

- [Part 1 — try it on your own machine](#part-1--try-it-on-your-own-machine) (5 minutes,
  no domain, no TLS, nothing installed system-wide)
- [Part 2 — run it for real](#part-2--run-it-for-real) (a server, a domain, about an hour)
- [If you have no public domain name](#if-you-have-no-public-domain-name)

---

## Part 1 — try it on your own machine

You need nothing but the binary. No Go toolchain, no Docker, no database.

### 1. Download it and check it is what it claims

Pick your platform from the [latest release](https://github.com/d-weber/almanack/releases/latest).
On Linux amd64:

```sh
VERSION=v0.2.0
curl -LO "https://github.com/d-weber/almanack/releases/download/$VERSION/almanack-$VERSION-linux-amd64"
curl -LO "https://github.com/d-weber/almanack/releases/download/$VERSION/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
mv "almanack-$VERSION-linux-amd64" almanack && chmod +x almanack
./almanack version
```

The other names are `linux-arm64`, `linux-armv7` (a 32-bit Raspberry Pi OS), `darwin-arm64`
and `darwin-amd64`. On macOS, `sha256sum` is `shasum -a 256`, and the first run needs
`xattr -d com.apple.quarantine almanack` because the binary is not notarised.

### 2. Write a throwaway configuration

```sh
cat > try.conf <<'EOF'
ALMANACK_DEV=true
ALMANACK_LISTEN=127.0.0.1:8080
ALMANACK_BASE_URL=http://localhost:8080
ALMANACK_DATA=./almanack.db
ALMANACK_TZ=Europe/Paris
ALMANACK_MAIL_DIR=./mail
EOF
```

`ALMANACK_DEV=true` is what makes this possible without TLS: it allows an `http://` base
URL, drops `Secure` from the session cookie so localhost works, writes email to files in
`./mail` instead of sending it, and mounts the developer endpoints at `/dev/`.

> **Development mode is not a way to run this for real.** `/dev/` includes an
> unauthenticated "sign in as anyone" link. Never set `ALMANACK_DEV` on a machine anything
> else can reach.

### 3. Fill it with a demo family and start it

```sh
./almanack --config try.conf seed
./almanack --config try.conf serve
```

Open <http://localhost:8080> and sign in as **mum@example.org** with the password
**password**. There is a month and an agenda view, a recurring swimming lesson with one
occurrence moved and another cancelled, a multi-day holiday and a birthday — enough to see
whether the thing suits you. Gran (`gran@example.org`) reads the same calendar in French.

Push notifications will not work here: browsers refuse Web Push on an insecure origin.
Everything else does.

To throw it away: delete `almanack.db`, `almanack.db-wal`, `almanack.db-shm`, `mail/` and
the binary. Nothing was installed.

---

## Part 2 — run it for real

### What you need before you start

| | Why |
|---|---|
| A machine that is always on | A Pi 4, a mini PC, a small VPS. Almanack idles at a few dozen MB. |
| **A domain name** pointing at it | HTTPS is not optional — Web Push and PWA installation both refuse an insecure origin, and Almanack rejects a non-`https://` base URL at startup. A subdomain of a domain you already own is fine. |
| Ports 80 and 443 reachable | For the certificate and for the app. At home this means a port forward on the router. |
| A mailbox you can relay through | Password resets, reminders by email, and the daily "everything is fine" mail all need it. Without mail, a failure on this server is silent. |
| Root on the machine | For the service user, the unit files and the proxy. |

If you cannot point a domain at the machine, read
[If you have no public domain name](#if-you-have-no-public-domain-name) first — it changes
step 4, and nothing else.

Throughout, replace `calendar.example.org` with your name and `you@example.org` with your
address.

### 1. Install the binary

```sh
VERSION=v0.2.0
ARCH=linux-amd64            # or linux-arm64, or linux-armv7 on a 32-bit Pi
cd /tmp
curl -LO "https://github.com/d-weber/almanack/releases/download/$VERSION/almanack-$VERSION-$ARCH"
curl -LO "https://github.com/d-weber/almanack/releases/download/$VERSION/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS

sudo install -m 0755 "almanack-$VERSION-$ARCH" /usr/local/bin/almanack
sudo useradd --system --home /var/lib/almanack --shell /usr/sbin/nologin almanack
sudo install -d -o almanack -g almanack -m 0750 /var/lib/almanack /var/lib/almanack/backups
sudo install -d -m 0755 /etc/almanack
```

### 2. Generate the push keys — once, ever

```sh
sudo -u almanack /usr/local/bin/almanack gen-vapid
```

Copy both lines into the config file below, and put a copy somewhere safe.

> **Never rotate these keys.** Every push subscription in every family member's browser is
> bound to the public key. Changing it silently stops notifications on every device until
> each person uninstalls and reinstalls the app. There is no migration path.

### 3. Write the configuration

`almanack.conf.example` in this repository documents every setting that exists; this is the
minimum that starts.

```sh
sudo tee /etc/almanack/almanack.conf > /dev/null <<'EOF'
ALMANACK_LISTEN=127.0.0.1:8080
ALMANACK_BASE_URL=https://calendar.example.org
ALMANACK_DATA=/var/lib/almanack/almanack.db
ALMANACK_BACKUP_DIR=/var/lib/almanack/backups
ALMANACK_TZ=Europe/Paris

ALMANACK_TRUSTED_PROXIES=127.0.0.1,::1

ALMANACK_SMTP=127.0.0.1:25
ALMANACK_MAIL_FROM=almanack@example.org
ALMANACK_OWNER_EMAIL=you@example.org
ALMANACK_HEARTBEAT_TIME=08:00

ALMANACK_VAPID_PUBLIC=<paste from gen-vapid>
ALMANACK_VAPID_PRIVATE=<paste from gen-vapid>
ALMANACK_VAPID_SUBJECT=mailto:you@example.org
EOF
sudo chown root:almanack /etc/almanack/almanack.conf
sudo chmod 0640 /etc/almanack/almanack.conf
```

`0640` matters: this file holds the VAPID private key.

`ALMANACK_TRUSTED_PROXIES` matters too. Behind a proxy every request appears to come from
the proxy, so without it the login rate limiter shares one bucket for the whole family and
one attacker hammering the login page locks everybody out.

`ALMANACK_PUSH_HOSTS` is not in the minimum above because its default is right for every
browser. A push subscription endpoint is a URL the browser gives to a device and this server
then posts to — the one address in the application that comes from outside and gets
dereferenced — so the hosts it may point at are restricted to the four push services
(Google's, Mozilla's, Apple's and Microsoft's). The cost of that is real: if a browser ever
starts handing out endpoints somewhere else, registering that device is refused with a 400
and the log says so by name —

```
WARN push endpoint refused: its host is not an allowed push service service=push.newvendor.example setting=ALMANACK_PUSH_HOSTS
```

— and the fix is to add the host to `ALMANACK_PUSH_HOSTS` (comma-separated; `*.example.org`
matches subdomains). Set it to `*` to accept any host, which is what running your own push
service needs.

If you modify Almanack and let other people use it, AGPL section 13 entitles them to your
source: set `ALMANACK_SOURCE_URL` to where it can be obtained and the app puts a link in
its About screen.

### 4. Put TLS in front of it

Almanack speaks plain HTTP on localhost and never terminates TLS itself.

**Caddy** is the least work, because it obtains and renews the certificate on its own:

```
# /etc/caddy/Caddyfile
calendar.example.org {
	reverse_proxy 127.0.0.1:8080
	request_body {
		max_size 4MB
	}
}
```

That is the whole file. Caddy already sends `X-Forwarded-For`, and adds no
`Content-Security-Policy` of its own — both of which matter (see below).

<details>
<summary>nginx, if you already run it</summary>

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name calendar.example.org;

    ssl_certificate     /etc/letsencrypt/live/calendar.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/calendar.example.org/privkey.pem;

    client_max_body_size 4m;    # avatar and calendar-picture uploads
    proxy_read_timeout 60s;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Get the certificate with `certbot --nginx -d calendar.example.org`.
</details>

Whatever you use, three rules:

1. **Pass `X-Forwarded-For`**, and list the proxy in `ALMANACK_TRUSTED_PROXIES`.
2. **Do not add a `Content-Security-Policy` header.** Almanack sets its own; a second one
   is intersected with it, and the UI breaks in ways that look like application bugs.
3. **Allow request bodies of at least 2 MB**, or image uploads fail.

### 5. Give it somewhere to post mail

Almanack speaks SMTP to a local MTA and relays nothing itself — deliberately, so that the
day your mail provider retires an authentication method it is an OS config edit rather than
an application rebuild. It needs something **listening on `127.0.0.1:25`**.

Postfix as a null client is the well-trodden option:

```sh
sudo apt install postfix     # choose "Internet Site" or "Satellite system" when asked
```

```sh
# /etc/postfix/main.cf — the lines that matter
inet_interfaces = loopback-only
mydestination =
relayhost = [smtp.example.com]:587
smtp_sasl_auth_enable = yes
smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd
smtp_sasl_security_options = noanonymous
smtp_tls_security_level = encrypt
```

```sh
echo '[smtp.example.com]:587 you@example.org:your-app-password' \
  | sudo tee /etc/postfix/sasl_passwd > /dev/null
sudo chmod 600 /etc/postfix/sasl_passwd
sudo postmap /etc/postfix/sasl_passwd
sudo systemctl restart postfix

# Prove it before going further.
echo "test" | mail -s "almanack mail test" you@example.org
```

If that message does not arrive, stop and fix it here. Nothing later in this guide will
tell you that mail is broken — that is the whole problem mail is solving.

`msmtpd` (from the msmtp package) is a lighter alternative that also listens on 25. Any MTA
that accepts on `127.0.0.1:25` and relays onward works.

### 6. Install the service

```sh
sudo tee /etc/systemd/system/almanack.service > /dev/null <<'EOF'
[Unit]
Description=Almanack shared calendar
Documentation=https://github.com/d-weber/almanack
After=network-online.target time-sync.target
Wants=network-online.target
OnFailure=almanack-alert@%n.service

[Service]
Type=notify
User=almanack
Group=almanack
EnvironmentFile=/etc/almanack/almanack.conf
ExecStart=/usr/local/bin/almanack serve
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
WatchdogSec=120s

StateDirectory=almanack
UMask=0077
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
RemoveIPC=true
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
```

`Type=notify` is not decoration: the process signals readiness **after** migrations finish
and the port is bound, so a restart that is still migrating is distinguishable from one
that has died, and `systemctl restart` does not report success on a service that is about
to exit because something else already holds the port. `WatchdogSec` restarts a scheduler
that has hung — which otherwise means reminders quietly stop while the process still
answers HTTP. The ping goes out once per completed scheduler tick, and a tick that runs
long delays the ping with it, so keep `ALMANACK_TICK` **under half of `WatchdogSec`** —
120 s against the default 30 s tick leaves four times the room it needs. The process warns
at startup when the tick passes that half, which is earlier than the point where it breaks:
a tick anywhere near the full `WatchdogSec` restart-loops the first time it runs slowly.
`After=time-sync.target` keeps a box that booted with a dead clock battery from either
flushing every pending reminder at once or marking them delivered without sending them.

### 7. Make failure loud

This is the most important step in the guide. A family server has no pager, and every other
component here fails silently.

```sh
sudo tee /etc/systemd/system/almanack-alert@.service > /dev/null <<'EOF'
[Unit]
Description=Mail the owner that %i failed

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'systemctl status --full --lines=50 %i | mail -s "almanack: %i failed on %H" you@example.org'
EOF
```

`OnFailure=` belongs in `[Unit]`. Put it under `[Service]` and systemd ignores it with a
warning you will never read, leaving you with alerting that is configured and does nothing.
`systemd-analyze verify /etc/systemd/system/almanack*.service` catches that and much else —
run it now.

### 8. Back it up on a timer

```sh
sudo tee /etc/systemd/system/almanack-backup.service > /dev/null <<'EOF'
[Unit]
Description=Almanack verified backup
After=almanack.service
OnFailure=almanack-alert@%n.service

[Service]
Type=oneshot
User=almanack
Group=almanack
EnvironmentFile=/etc/almanack/almanack.conf
ExecStart=/usr/local/bin/almanack backup /var/lib/almanack/backups --prune
EOF

sudo tee /etc/systemd/system/almanack-backup.timer > /dev/null <<'EOF'
[Unit]
Description=Hourly Almanack backup

[Timer]
OnCalendar=hourly
RandomizedDelaySec=5m
Persistent=true

[Install]
WantedBy=timers.target
EOF
```

`almanack backup` verifies each snapshot's integrity and **exits non-zero if it is not
intact** — which is why the `OnFailure=` line above is the one that tells you your database
is damaged while the good generations still exist.

Snapshots on the same disk are not backups. Sync `/var/lib/almanack/backups` off the machine
at least daily, copying only `almanack-*.db` and never `*.tmp`.

### 9. Start everything

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now almanack.service almanack-backup.timer
sudo systemctl status almanack.service
curl -s http://127.0.0.1:8080/healthz | head -20
```

`/healthz` returns `200` with `"status":"ok"`, or `503` when something is degraded. It
reports database reachability, scheduler heartbeat age, last backup age and result, SMTP
failures, per-push-service errors and disk usage — it is what to point any monitoring at.

Then check the real address in a browser: <https://calendar.example.org>.

### 10. Create the first account

Signup is invite-only and there is no HTTP route to the first account, so this happens on
the machine:

```sh
sudo -u almanack /usr/local/bin/almanack --config /etc/almanack/almanack.conf \
  bootstrap --email you@example.org --name "Your name"
```

It prints a generated password and a reusable invite link valid for seven days. Sign in,
change the password immediately, then send the link to everyone else. Each person picks
their own name, colour and language as they join.

On a phone, open the site and use "Add to Home Screen" — that is the install, and it is what
enables push notifications. On iOS it only works from Safari, and only once installed.

### 11. Confirm it is actually working

- [ ] `https://calendar.example.org` loads and you can sign in.
- [ ] `systemctl status almanack` is `active (running)`.
- [ ] `curl -s https://calendar.example.org/healthz` says `"status":"ok"`.
- [ ] You received a password-reset mail after asking for one (proves outbound mail).
- [ ] `systemctl list-timers almanack-backup.timer` shows a next run.
- [ ] `ls /var/lib/almanack/backups` has a snapshot after the first hour.
- [ ] **The daily heartbeat mail arrives the next morning.** If it does not, your monitoring
      is not working, and everything above this line will eventually fail without telling you.
- [ ] You have rehearsed a restore once — see [RESTORE.md](RESTORE.md).

Keep off the machine: the config file, a copy of the release binary for your architecture,
your registrar and DNS credentials, and a note of the router port forwards. Snapshots alone
will not rebuild this server.

---

## If you have no public domain name

HTTPS is a hard requirement, and browsers will not accept a self-signed certificate for a
service worker. Two ways out, both fine:

**A domain you own, resolving to a private address.** Point `calendar.example.org` at
`192.168.1.x` in public DNS and let Caddy get a certificate over the DNS-01 challenge, which
never needs the machine to be reachable from the internet. Needs a Caddy build with your DNS
provider's module.

**Tailscale.** `tailscale cert` and `tailscale serve` give a real, trusted certificate on a
`*.ts.net` name with no ports open to the internet at all. Set `ALMANACK_BASE_URL` to the
`https://…ts.net` name. Everyone who uses the calendar has to be on the tailnet, which for a
household is often what you wanted anyway.

What will **not** work: a bare IP address, `http://`, a self-signed certificate, or a
`.local` mDNS name. The app rejects the first two at startup, and the browser rejects the
others when the service worker tries to register.

---

## Upgrading

```sh
VERSION=v0.3.0
curl -LO "https://github.com/d-weber/almanack/releases/download/$VERSION/almanack-$VERSION-linux-amd64"
curl -LO "https://github.com/d-weber/almanack/releases/download/$VERSION/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
sudo install -m 0755 "almanack-$VERSION-linux-amd64" /usr/local/bin/almanack
sudo systemctl restart almanack
```

Migrations run at startup, and a release that has any writes a snapshot of the database
first, into `<ALMANACK_BACKUP_DIR>/pre-migration/`. It refuses to start if it cannot write
one.

**Rolling back means restoring that snapshot**, not just putting the old binary back. A
binary refuses to start against a schema newer than it knows — so the older one will not
open a database the newer one migrated, which is what makes a mistaken downgrade fail
loudly instead of corrupting anything, and also what makes the snapshot the way back.
Everything the family entered after the upgrade is lost with it, so roll back sooner
rather than later or not at all.

Open browsers pick up the new version by themselves: every response carries the app version
and the client reloads when it changes.

**Check your `ALMANACK_*` variables before you restart.** Configuration is strict: a key the
binary does not recognise is a startup error naming it, rather than a setting silently
ignored. From 0.3.0 that applies to the environment as well as to the config file — which is
the same thing under `EnvironmentFile=`, and was the gap that left the check doing nothing in
this deployment. So a setting a release removes or renames has to come out of everywhere it
is set, not just out of `/etc/almanack/almanack.conf`: `systemctl show almanack -p Environment`
for anything the unit or a drop-in adds, and any profile script that exports one. The
changelog names every setting a release removes. If a restart fails, the error says which key
and where it was seen; nothing has started and nothing has changed.

## When something is wrong

| Symptom | Where to look |
|---|---|
| `almanack: configuration problems:` on start | It lists every problem at once, by setting name. Nothing started; fix them and try again. |
| `… is not a setting this version understands` | An `ALMANACK_*` key this version does not have, in the config file or in the service's environment. The message says which and where. It is usually a typo; after an upgrade it is a setting the new version dropped. |
| Service restarts in a loop | `journalctl -u almanack -n 50`. Almost always the config file or permissions on `/var/lib/almanack`. |
| The page loads but nothing works | A second `Content-Security-Policy` from your proxy. Remove it. |
| Uploads fail | The proxy's body size limit; needs at least 2 MB. |
| No push notifications | The app must be installed to the home screen, not just open in a tab. iOS requires Safari and an install. Check `/healthz` for push errors. |
| One device refuses to register for push | `journalctl -u almanack \| grep "push endpoint refused"`. Its browser hands out endpoints on a host `ALMANACK_PUSH_HOSTS` does not allow; the line names it. |
| No email at all | Test the MTA directly with `mail`. Almanack never relays; if `mail` does not work, neither will it. |
| Everyone locked out after one wrong password | `ALMANACK_TRUSTED_PROXIES` does not list your proxy, so the whole family shares one rate-limit bucket. |
| Times are an hour out | `ALMANACK_TZ` is the family timezone and the only one that matters; devices elsewhere still see family time. |

Anything else: [the issue tracker](https://github.com/d-weber/almanack/issues) lists what is
known to be broken, and new issues are welcome.
