# Security policy

## Reporting a vulnerability

Please report privately, through GitHub's **Report a vulnerability** button on the
Security tab of this repository, rather than opening a public issue.

Include what you did, what happened, and what you expected. A proof of concept helps but
is not required — a clear description of the flaw is enough to start.

This is a hobby project maintained by one person, so please do not expect a same-day
reply. You should get an acknowledgement within a week and an assessment within two.

## Scope

Agenda is designed to be reachable from the public internet, so anything that lets someone
read or change a group's calendar without being invited to it is in scope. In particular:

- authentication, session handling, invite and password-reset tokens
- authorisation between calendars — a member of one group reaching another group's data
- injection of any kind, including stored content rendered in the browser
- the upload paths (avatars and calendar pictures)
- anything that lets a request escape the configured data directory

Out of scope, because they are deliberate design decisions documented in
[docs/architecture.md](docs/architecture.md):

- **Flat permissions inside a calendar.** Every member can edit and delete anything in a
  calendar they belong to. This is a tool for people who already trust each other.
- **No end-to-end encryption.** The server can read the calendar; anyone with the database
  file can too.
- **Denial of service by an authenticated member.** There are no per-user quotas, because
  the expected deployment is a household.
- Findings that require an attacker who already has the server's filesystem or the config
  file, which contains the push private key.

## What operators should know

Two things carry most of the practical risk in a deployment, and neither is a code flaw:

1. **The config file holds the VAPID private key.** Deploy it `0640`, not world-readable.
2. **Failure alerting is the security control that matters most here.** A family server
   with nobody watching it fails silently. See [docs/deployment.md](docs/deployment.md).

## Supported versions

The project is at 0.x. Only the latest commit on the default branch is supported; there
are no backports.
