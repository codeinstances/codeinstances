# Notes for Claude

This file says where the next session starts and what comes next. What comes
next is decided with the owner before it goes here, and work starts only when
the owner says go.

## Releases

Releases are automated. To release, raise `Version` in `codeinstances.go` and
push to `main`; never ask the owner to publish one. [RELEASING.md](RELEASING.md)
has the procedure and the checks to run afterwards.

## Where things stand

v0.1.0 to v0.6.0 are the releases so far (all October 2026):
Apache 2.0, CI on Linux and macOS plus a Windows smoke check, and automated
releases with smoke-tested binaries for Linux, macOS and Windows. Pure Go,
standard library only, no cgo.

The idea: every run of a program is an instance, an object you can keep and do
things to, the way an object is an instance of a class. v0.1.0 does the first
two verbs, keep and replay: `codeinstances record` runs any program behind a
local HTTP(S) proxy with a per-run certificate authority and keeps the run as
an instance file; `codeinstances replay` reruns it offline against the
recording and reports divergences; `show` and `verify` inspect files.
v0.2.0 adds the third verb, compare: `codeinstances diff a b` (`compare.go`)
pairs the calls of two runs and reports how they ended, their output, their
calls and values, and their speed. Calls carry `duration_ms` from 0.2.0 on
(an additive member). Cost is on hold at the owner's word (October 2026):
no money, token or byte accounting until he says otherwise. v0.3.0 adds
shrink and test: `codeinstances shrink -pass a b` (`shrink.go`) finds by delta
debugging the fewest of the failing run's answers that keep the failure,
swapping in the passing run's answers, and writes that instance;
`codeinstances test` (`regression.go`) replays instances as regression tests
(recorded exit 0 must match, a recorded failure must now exit 0). v0.4.0
adds group: `codeinstances group` (`group.go`) fingerprints failing runs
(ended, normalised error line or Go panic / Python traceback, failed calls)
and groups equal fingerprints; `show` prints a failing run's fingerprint.
v0.5.0 adds share and store: the `store/` package (a folder of
`<hex>.instance` files, an HTTP server with a bearer token, a client),
`codeinstances serve`, `push`, `pull`, and `report` (`report.go`, a Markdown
bug report); docs/ci.md is a GitHub Actions example (not run by our CI). v0.6.0 adds
fork: `codeinstances fork -at N` (`fork.go`; the proxy's forking mode in
`internal/capture`) answers the parent's calls before step N and runs live
(or `-offline`, or with an `-answer` given at the step) from there; forks
carry `fork` {parent, at} and calls `from_seq` / `given` (additive members).
The Go
`hook` package records and replays clock and random values. The open file
format is [docs/FORMAT.md](docs/FORMAT.md) (`codeinstances/0`); change it only
with a new format string or additive members, and keep
[docs/example.instance](docs/example.instance) valid (a test checks it).

Layout: `codeinstances.go` (Record, Replay, comparison), `store/` (store,
server, client), `instance/` (format,
canonical id, atomic save), `internal/capture/` (the proxy), `internal/ca/`
(per-run authority), `hook/`, `cmd/codeinstances/`, `examples/demo/` (the
self-checking demo), `scripts/` (smoke check, release plan, record-tests).

The README's "Limits in v0.6.0" section is the honest list of what isn't
covered; keep it true.

## Long-range plan (agreed with the owner, October 2026)

Each phase adds one verb, ships as its own release and stands on its own:
1. Keep and replay (done in v0.1.0).
2. Compare: diff two instances, passing against failing, with speed (done in
   v0.2.0; cost on hold).
3. Shrink a failing instance to the smallest that still fails (delta
   debugging); export instances as regression tests (done in v0.3.0, shrink
   against a passing run).
4. Group: fingerprint failing instances so repeats collapse into one bug
   (done in v0.4.0).
5. Share and store: stable IDs, shareable runnable bug reports, a simple
   self-hosted store, CI integration (done in v0.5.0).
6. Fork an instance at a step and run on from there (done in v0.6.0).
7. Proof: signed, tamper-evident instances (AI agent accountability, audit).
8. Run instances on demand (FaaS sense). Optional, the heaviest.

Each release gets a plain-language post on the site. Agents are a use case,
never the whole identity.

## Test results

`sh scripts/record-tests.sh` runs every test, the race detector, the release
plan's cases, the demo (10 replays) and the smoke check on release-style
binaries, and writes them to `test/results/`. Every number on the site and in
the README comes from those files; record again after any change that could
move them. It needs Go and Python 3. In a cloud session, fetch modules with
`GOPROXY=direct` if needed (there are none outside the standard library today).

## Site

CodeInstances.com is in `photozeitgeist/codeinstances.com`: the Hugo Launch
template copied unchanged, with content and config values only. Pages: about,
contact, quick start, download, reference. Posts: the launch, the category
explainer, how it was tested, the name, AI agents, the open format, and five
use scenarios tagged "case studies" (a flaky CI test, a runnable bug report,
an AI agent run, a dependency upgrade, a metered API). Scenarios are
illustrative: no invented customers or results. Images are drawn into
static/images.
Every number comes from `test/results/`; when a recording changes a number,
update the site too.

SoftDevStudio.com (`photozeitgeist/softdevstudio.com`) lists CodeInstances as
its fourteenth project. Each release gets a short note there, rephrased from
the site's posts, following that repo's CONTENT-PLAN.md (2.8).

## Next

Phase 7 (proof: signed, tamper-evident instances) is next on the plan but
not agreed yet. Agree it with the owner before it goes here.
