# Contributing

Thanks for looking. This is a small project with strong opinions, and the opinions are
written down so that a pull request never has to guess at them.

## Before anything else

Read [CONVENTIONS.md](CONVENTIONS.md). It is short and it is binding — dependency policy,
the rules that keep dates correct, the `h()` contract in the browser, and how migrations
work. Most review comments a change would otherwise attract are already answered there.

[docs/architecture.md](docs/architecture.md) explains *why* those rules exist.

## Getting set up

Go 1.25 or newer, and nothing else.

```sh
make seed && make dev     # http://localhost:8080 — mum@example.org / password
make check                # gofmt + go vet + the whole test suite
```

`make check` is the gate. It must be green before a change is ready, and CI runs the same
thing.

[docs/development.md](docs/development.md) covers the parts that are not obvious: testing
notifications with no push service and no mail server, travelling through time to make a
reminder fire, and rehearsing a backup restore.

## What is likely to be accepted

[The issue tracker](https://github.com/d-weber/almanack/issues) is the to-do list. Every
item there was reproduced before it was filed, and each one says where the code is and how
big the fix should be; [`good first issue`](https://github.com/d-weber/almanack/labels/good%20first%20issue)
marks the small ones. The [milestones](https://github.com/d-weber/almanack/milestones) say
what is wanted in which release.

- Bug fixes, with a test that fails before the fix.
- Translations. The catalogs are `internal/i18n/locales/*.json`; a test asserts every
  language has exactly the same key set, and it will tell you which keys are missing.
- Accessibility and small UI improvements, especially on phones.
- Documentation that corrects something wrong or unclear.
- Platform fixes for push, which is the part most likely to drift.

## What is likely to be declined

Not because the ideas are bad, but because they change what this is:

- **New dependencies.** Two direct Go modules, zero in the browser. If a change needs a
  third, it needs a strong argument first, in an issue, before the code.
- **A frontend framework or a build step.** The only build is `go build`.
- **CalDAV, ICS, or Google/Outlook sync.** Deliberately out of scope; see the architecture
  document. A separate project that syncs against the HTTP API would be welcome — knowing
  that the API is the seam this app's own browser code speaks rather than a frozen
  contract, and changes with releases. Pin one, read the changelog, expect to follow.
  [docs/api.md](docs/api.md) says what that does and does not promise.
- **Chat, photo albums, shared lists.** Also deliberate omissions.
- **Multi-tenancy or a scaling story.** This is for one group on one server.

Open an issue before writing anything large. It is nobody's idea of fun to decline a
finished pull request.

## Pull requests

- One concern per pull request.
- New behaviour arrives with its test; a bug fix arrives with the failing case first.
- Keep `gofmt` clean and `go vet` quiet — `make check` covers both.
- Match the surrounding style, including comment density. Comments here explain *why*
  something is the way it is, not what the next line does.
- If you touch dates, recurrence or notifications, say in the description which existing
  tests you ran and which cases you added. Those areas are where this project has been
  burned before, and the tests are unusually specific for that reason.
- If you add a JavaScript module, add it to the precache list in `web/sw.js`. Forgetting
  breaks offline use quietly.

## Reporting bugs

Use the issue templates. For a calendar bug, the three things that make it reproducible are
almost always: the timezone, whether the event is all-day or timed, and whether it is part
of a recurring series.

Security issues go through [SECURITY.md](SECURITY.md) instead — please do not open a public
issue for those.

## Licence

Contributions are accepted under the [AGPL-3.0](LICENSE), the same licence as the project.
