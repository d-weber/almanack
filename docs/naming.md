# Naming

A survey of the open-source shared-calendar landscape, and the reasoning behind the
project's name.

## 1. What already exists

Star counts pulled from the GitHub API, July 2026.

### Cluster A — CalDAV plumbing

Sync servers. No shared-calendar UX; you bring your own client.

| Project | Stars | Language |
|---|---|---|
| [Radicale](https://github.com/Kozea/Radicale) | 4,851 | Python |
| [Baïkal](https://github.com/sabre-io/Baikal) | 3,242 | PHP |
| [SabreDAV](https://github.com/sabre-io/dav) | 1,715 | PHP |
| [Davis](https://github.com/tchapi/davis) | 725 | PHP |
| [Xandikos](https://github.com/jelmer/xandikos) | 592 | Python |
| DAViCal | 108 | PHP |

### Cluster B — groupware suites

Calendar is one feature among twenty.

| Project | Stars | Language |
|---|---|---|
| [SOGo](https://github.com/Alinto/sogo) | 2,139 | Objective-C |
| [Nextcloud Calendar](https://github.com/nextcloud/calendar) | 1,166 | JavaScript |
| [Kurrier](https://github.com/kurrier-org/kurrier) | 1,001 | TypeScript |

### Cluster C — booking / scheduling

Calendly-shaped. Solves "find me a slot", not "what is the family doing Saturday".

| Project | Stars | Language |
|---|---|---|
| [Cal.com](https://github.com/calcom/cal.com) | 46,807 | TypeScript |
| [Easy!Appointments](https://github.com/alextselegidis/easyappointments) | 4,276 | PHP |

### Cluster D — the actual neighbours

Shared/family calendars with a real UI. This is the only cluster we compete in, and it
is thin.

| Project | Stars | Language | Note |
|---|---|---|---|
| [keeper.sh](https://github.com/ridafkih/keeper.sh) | 1,211 | TypeScript | sync tool + calendar MCP server |
| [Yuvomi](https://github.com/ulsklyc/yuvomi) | 1,047 | JavaScript | family planner, zero build step |
| FamilyHub | <100 | React 19 | touchscreen family hub |
| [ical-git](https://github.com/revelaction/ical-git) | 3 | Go | notifier over a dir of `.ics` |

### The gap

**There is no well-known self-hosted TimeTree.** The calendar space is DAV plumbing or
Calendly clones; the group-calendar-with-a-UI niche is served by a handful of young
projects. Yuvomi reached ~1k stars on essentially our thesis (self-hosted, family, no
build step) with a far weaker longevity story, which is decent evidence the demand is
real and unclaimed.

## 2. What the survey says about naming

The incumbents are named for infrastructure, not for people:

- **Protocol mash** — DAViCal, SabreDAV. Reads as plumbing.
- **Opaque place names** — Radicale, Baïkal. Brandable, but say nothing.
- **Function-literal** — `calendar-server`, Easy!Appointments. No personality.
- **Invented brandables** — Yuvomi, Kurrier. Memorable, meaningless.
- **Etymology play** — Xandikos (a month in the ancient Macedonian calendar). Precedent
  that this register works in this space.

Two conclusions:

1. **Nobody owns a warm, human, real-word name here.** That lane is open.
2. **Avoid `cal*`.** Cal.com owns it with 46k stars, and the prefix is saturated
   (Calendso, Calendara, calendar-server, DAViCal, Radicale).

## 3. Why not "Agenda"

`agenda/agenda` is a **9,689★ Node.js job scheduler**. `tc39/agendas`, `agendash`,
`AgendaCalendarView` all crowd the term further. Searching "agenda github" will never
surface this project. The name has to change.

## 4. Candidates

Verified against the GitHub API and registry RDAP, July 2026.

### Kalends — recommended

Latin *kalendae*: the first day of the Roman month, and the root the word *calendar* is
derived from. It was the day the pontifex **called out** (*calare*) the month's schedule
to the assembled people — literally the original act of announcing a shared schedule to
a group. It is also the day debts came due, which is why *kalendarium* meant an account
book, which is how we got "calendar".

- **Namespace:** clean. Three repos match, all 0★.
- **Domains:** `kalends.day` ✅ and `kalends.page` ✅ available. `.com/.org/.dev/.app`
  parked.
- **Binary:** `kalends`. Config: `kalends.conf`. Reads fine.
- **Risk:** the K. Mitigated — it makes the term uniquely searchable, and the K *is* the
  hook that makes a developer look it up.

### Almanack — runner-up

Poor Richard's Almanack. An almanac is a printed table of what happens when, built to
stay useful for a whole year with no updates — the longevity thesis in one word. The
archaic `-ck` spelling keeps it distinctive.

- **Namespace:** near-clean, top match 54★.
- **Domains:** `almanack.dev` ✅, `almanack.app` ✅.
- **Risk:** evokes weather and farming more than scheduling. Eight characters.
- **Upside:** the most legible name here to a non-technical person, which matters for
  a family calendar.

### Gnomon — strong word, real collision

The shadow-casting rod of a sundial. Oldest timekeeping instrument there is, no moving
parts, works for millennia. Excellent metaphor, excellent word.

- **Blocked by:** `paypal/gnomon` at 932★ (a log-timestamping tool). `gnomon.app` and
  `gnomon.dev` both taken.

### Also considered

| Name | Why it's interesting | Why not |
|---|---|---|
| **Fasti** | The Roman public calendar, carved in stone in the forum — the state's shared schedule as a monument. `fasti.dev` free | Sits inside "Fastify" (36.8k★). Search collision is fatal |
| **Kith** | "Kith and kin" — literally *the people you know*, matching the README's own phrase. Four letters | KITH is a major streetwear brand. SEO unwinnable |
| **Althing** | The Icelandic assembly, sitting since 930 AD. Best longevity story available | "thing" is a bad substring in software; says nothing about time |
| **Klepsydra** | Water clock. `klepsydra.dev` free, namespace near-clean | Hard to spell and say. Klepsydra Technologies exists |
| **Horologe** | Archaic "timepiece". `horologe.dev` free, namespace clean | Obscure without being evocative |
| **Analemma** | The figure-eight the sun traces over a year. Beautiful | Nobody can spell it. `liebke/analemma` 183★ |
| **Perpetua** | From "perpetual calendar"; Latin for everlasting | `perpetual-ml/perpetual` 702★ plus two DeFi projects |
| **Quorum** | Enough people present to decide | Consensys/quorum, 4,767★ |
| **Equinox** | — | 6,782★ and 2,928★ collisions |
| **Sundial** | — | SUNDIALS (LLNL) 675★; generic |
| **Hearth** | — | Hearthstone owns the space |
| **Ephemeris** | A table of positions over time — apt | Astronomy and astrology own the term |

## 5. Recommendation

**Kalends**, with `kalends.day` as the domain.

It is the etymological root of the thing the project is, the namespace is clean, the
domain is excellent, and it is a nerd-snipe: a developer either already knows where
"calendar" comes from and enjoys the reference, or looks it up and learns something.
Both are small gifts, and small gifts get upvoted.

One caveat worth stating plainly: **the name is maybe a tenth of the stars.** The pitch
does the rest, and this project's pitch is unusually strong. Lead with it:

> **Kalends** — the Roman day for announcing the month's schedule.
> A self-hosted shared calendar in one Go binary. Two dependencies, zero JavaScript
> dependencies, no build step, no Docker required. SQLite on disk. Designed to still be
> running in ten years.

Suggested HN / r/selfhosted title:

> Kalends: a self-hosted shared calendar in one Go binary, two dependencies, no build step
