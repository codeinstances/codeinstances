# CodeInstances

Keep a run of any program as a file, and replay it.

Your code is versioned, reviewed and stored. Its runs are thrown away: a
test fails in CI, the rerun passes, and the bug is gone. CodeInstances keeps
the run. `codeinstances record` runs a program and keeps everything that made
that run what it was: its arguments, what it printed, how it ended, every
HTTP and HTTPS exchange it made, and every clock or random value it asked for
through the hook package. The result is an instance, one JSON file in an open
format. `codeinstances replay` runs the program again with the network
replaced by the recording and tells you whether it came out the same.

**Status: v0.1.0, an early release.** Recording, offline replay, divergence
reports, the Go hook package, the open format and its integrity check work and
are tested on Linux and macOS, with the release binaries also checked on
Windows. The limits are listed [below](#limits-in-v010). Open source under the
Apache License 2.0.

## Download

Ready-built binaries for Linux (x86-64 and ARM64, static), macOS (Apple silicon
and Intel) and Windows (x86-64) are on the
[releases page](https://github.com/codeinstances/codeinstances/releases/latest).
Each archive holds `codeinstances` and `codeinstances-demo`, the self-checking
demo. These links always fetch the latest release:

| System | Archive |
|---|---|
| Linux, x86-64 | [codeinstances_linux_amd64.tar.gz](https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_linux_amd64.tar.gz) |
| Linux, ARM64 | [codeinstances_linux_arm64.tar.gz](https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_linux_arm64.tar.gz) |
| macOS, Apple silicon | [codeinstances_darwin_arm64.tar.gz](https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_darwin_arm64.tar.gz) |
| macOS, Intel | [codeinstances_darwin_amd64.tar.gz](https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_darwin_amd64.tar.gz) |
| Windows, x86-64 | [codeinstances_windows_amd64.zip](https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_windows_amd64.zip) |

SHA256SUMS on the release page lists the checksums. Every archive was
smoke-tested on its own kind of machine before the release went out (the
Intel macOS build is checked for architecture only). The macOS binaries aren't
signed by Apple; run `xattr -d com.apple.quarantine codeinstances
codeinstances-demo` if macOS stops them. To build from source:
`go install github.com/codeinstances/codeinstances/cmd/codeinstances@latest`.

## Quick start

Record any program. Here, a Python one-liner:

```sh
codeinstances record -o weather.instance -- python3 fetch_weather.py
```

Its output passes through as usual, and the run is kept in `weather.instance`.
`codeinstances record` exits with the program's own exit code, so it drops
into scripts and CI unchanged.

```sh
codeinstances show weather.instance      # what ran, how it ended, every call
codeinstances replay weather.instance    # run it again, offline
```

Replay never opens a connection. Each request the program makes is answered
from the recording, so the replay works with the network off, the API down or
the test data changed. At the end you get either

```
codeinstances: replay matched (3 of 3 calls served, 0 of 0 values used)
```

or the ways the run differed:

```
codeinstances: replay diverged in 2 ways:
  - request GET https://api.example.com/v2/forecast isn't in the recording
  - recorded call 2 (GET https://api.example.com/v1/forecast) was never made
```

`replay` exits 0 when the run matched and 1 when it diverged. To check a fix
against a recorded failure, replay a different command against the same file:

```sh
codeinstances replay failing.instance -- ./myprogram-fixed --same --flags
```

### Commands

| Command | What it does |
|---|---|
| `record [flags] -- command [args...]` | Run the command and keep the run (`-o` names the file, `run.instance` by default) |
| `replay [flags] file [-- command [args...]]` | Run it again against the recording and compare |
| `show [-json] file` | Describe an instance |
| `verify file` | Check the file hasn't been changed since it was recorded (exit 1 if it has) |
| `version [-json]` | The version, the format version and the commit it was built from |

`record` flags: `-env NAME` keeps an environment variable and sets it again on
replay; `-resolve host=addr` sends a host name to another address, like curl's
`--resolve`; `-upstream-ca file.pem` trusts extra certificates when reaching
real servers; `-keep-secrets` keeps the headers that are redacted by default;
`-timeout 30s` kills the program after that long; `-q` hides its output.

`replay` flags: `-strict` requires HTTP requests in exactly the recorded order
(by default each request gets the next recorded answer for the same method,
URL and body, whatever order threads send them in); `-ignore-stderr` leaves
stderr out of the comparison; `-json` prints the report as JSON; `-timeout`
and `-q` as above.

## How it works

`record` starts a proxy on a loopback port and runs the program with
`HTTP_PROXY` and `HTTPS_PROXY` pointing at it. Plain HTTP requests are passed
on and kept. For HTTPS, the proxy stands in for the server with a certificate
from a certificate authority made for that run alone; the program is told to
trust it through `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE` and
`NODE_EXTRA_CA_CERTS`, which most HTTP clients read. The authority's key stays
in memory and is gone when the run ends. The proxy reaches the real servers
with your system's trusted certificates, and through your own proxy if your
environment sets one.

`replay` starts the same kind of proxy, with a fresh authority, that answers
from the recording and never dials out. A request with no recorded answer gets
a 502 and goes in the report, and so does every recorded request the program
didn't make.

By default the values of `Authorization`, `Proxy-Authorization`, `Cookie`,
`Set-Cookie` and the common API key headers are replaced with `[redacted]` in
the file. Request and response bodies are kept as they were.

The instance file is written to a temporary file and renamed into place, so a
recorder that is killed leaves no half-written file. Ctrl-C goes to the
program; the recorder waits for it to finish and keeps what happened.

## Clock and random numbers in Go

A program's HTTP traffic can be seen from outside. Its clock and its random
numbers can't, so a Go program that wants exact replay asks for them through
the hook package:

```go
import "github.com/codeinstances/codeinstances/hook"

start := hook.Now()          // instead of time.Now()
id := hook.Uint64()          // instead of math/rand or crypto/rand
pick := hook.Intn(10)
buf := make([]byte, 16)
hook.Read(buf)
client := hook.Client()      // an http.Client that trusts the run's authority
```

Outside codeinstances these are the real clock and `crypto/rand`, with no
files written. Under `record` each value is kept in the instance; under
`replay` the recorded values come back in the same order. A program that asks
for more values than were recorded gets fresh ones and the replay is reported
as diverged; so is one that asks for fewer.

## The instance format

An instance is one JSON object: the command, the result (exit code or signal,
stdout and stderr, duration), the calls (each request and response, or the
error when none came back) and the values. [docs/FORMAT.md](docs/FORMAT.md)
describes every member, and [docs/example.instance](docs/example.instance) is
a real one. Each file carries an id, the SHA-256 of its canonical form, so
`codeinstances verify` notices any edit.

## The demo and how it was tested

```sh
codeinstances-demo > report.json
```

The demo starts two local servers, one HTTPS and one plain HTTP, whose answers
change on every request. It records a Go program that makes 23 requests to
them, 20 of them from parallel goroutines, and takes 5 clock and random values
through the hook package. Then it stops both servers and replays the instance
10 times. Every replay has to match. It then replays two changed programs (one
makes an extra request, one prints the real clock) and checks both are caught,
and checks `verify` catches an edited file. Progress goes to stderr and a JSON
report to stdout; any failed check makes it exit 1.

`sh scripts/record-tests.sh` runs every test, the race detector, the demo and
the release smoke check, and writes them to [test/results](test/results). From
the latest recording, on a 2-CPU Linux machine (details in the files):

- 51 Go tests pass, and the race detector finds nothing
  ([tests.txt](test/results/tests.txt), [race.txt](test/results/race.txt)).
- The demo's 11 checks pass. Recording the 23 calls took 55 ms; the 10 offline
  replays took 26 to 35 ms each; the instance file was 20,066 bytes
  ([demo.json](test/results/demo.json)).
- The smoke check passes on release-style binaries, including recording and
  replaying a Python program that knows nothing of CodeInstances
  ([smoke.txt](test/results/smoke.txt)).

The tests cover the format (round trips, the id catching edits, atomic saves),
the proxy (HTTPS and HTTP replayed with the servers shut down, 64 concurrent
requests replayed three times, strict order, 4 and 8 MiB bodies, redaction,
upstream errors replayed as the same 502), changed programs (an extra request,
a missing request, an extra random value, an unhooked clock), crashes, killed
programs and timeouts, a recorder killed with SIGKILL leaving no file, and
Ctrl-C reaching the program while the recorder finishes.

## Limits in v0.1.0

- HTTP only. WebSocket, raw TCP and other protocols aren't recorded.
- Bodies are buffered whole, up to 64 MiB each, so a streamed response reaches
  the program all at once.
- The program gets an empty stdin.
- Clock and random values are captured only for Go programs that use the hook
  package. For other languages, HTTP is captured and the clock isn't.
- Programs that ignore `HTTP_PROXY` and `HTTPS_PROXY` aren't captured.
- Go's HTTP client never uses a proxy for `localhost` or `127.0.0.1`. A Go
  program talking to a local server should use a host name, and `-resolve`
  sends that name to the local address.
- Programs that pin certificates won't accept the run's authority.
- On macOS and Windows, Go ignores `SSL_CERT_FILE`, so a Go program needs
  `hook.Client()` (or `hook.Transport()`) for HTTPS.
- Secrets inside bodies aren't redacted.
- Output is kept up to 16 MiB per stream.

## Where it goes next

CodeInstances treats each run as an object you can do things to. This release
does two of them, keep and replay. Comparing two runs, shrinking a failing run
to the smallest one that still fails, turning recorded runs into regression
tests, grouping repeated failures, and signed instances as proof of what ran
are on the plan, in that rough order.

## Building and contributing

```sh
go build ./cmd/codeinstances
go test ./...
```

No dependencies outside the Go standard library. [RELEASING.md](RELEASING.md)
explains how releases go out. CodeInstances is a
[SoftDevStudio.com](https://softdevstudio.com) project;
[codeinstances.com](https://codeinstances.com) has more.

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE) and
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
