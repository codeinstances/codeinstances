# CodeInstances

Keep a run of any program as a file, replay it, compare it with another,
shrink a failure to what causes it, keep it as a test, group repeats of the
same bug, share it through a store of your own, fork it at a step to run on
from there, sign it so others can check who kept it and that nothing
changed since, and have a store run it on demand.

Your code is versioned, reviewed and stored. Its runs are thrown away: a
test fails in CI, the rerun passes, and the bug is gone. CodeInstances keeps
the run. `codeinstances record` runs a program and keeps everything that made
that run what it was: its arguments, what it printed, how it ended, every
HTTP and HTTPS exchange it made, and every clock or random value it asked for
through the hook package. The result is an instance, one JSON file in an open
format. `codeinstances replay` runs the program again with the network
replaced by the recording and tells you whether it came out the same.
`codeinstances diff` puts two runs side by side, a passing one and a failing
one say, and shows where they part ways and which calls got slower.
`codeinstances shrink` narrows a failure down to the few answers that cause
it, `codeinstances test` replays a folder of instances as regression tests,
`codeinstances group` puts failing runs that fail the same way together, and
`codeinstances serve`, `push`, `pull` and `report` share runs through a
self-hosted store and as bug reports, `codeinstances fork` takes a run up
to a step and runs on from there, live or with an answer you give, and
`codeinstances sign` and `verify -trust` show who kept a run and that it
wasn't changed afterwards, and `codeinstances run` has a store replay a
signed run, or try a what-if on it, on demand.

**Status: v0.8.0, an early release.** Recording, offline replay, divergence
reports, comparing two runs, shrinking a failure, instances as regression
tests, grouping failures, a self-hosted store, bug reports, forking a run,
signed runs, runs on demand, the Go hook package, the open format and its integrity check work and are
tested on Linux and macOS, with the release binaries also checked on
Windows. The limits are listed [below](#limits-in-v080). Open source under the
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
into scripts and CI unchanged. (A program that exits 2 is then hard to tell
from a usage error; the instance file is there either way.)

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

`replay` exits 0 when the run matched and 1 when it diverged.

Replay runs the command stored in the file. Treat an instance from someone
else like a script from someone else: run `codeinstances show` on it first to
see what it will run, or give your own command after `--`.

To check a fix
against a recorded failure, replay a different command against the same file:

```sh
codeinstances replay failing.instance -- ./myprogram-fixed --same --flags
```

### Comparing two runs

Record the run that works and the run that doesn't, then compare them:

```sh
codeinstances diff passing.instance failing.instance
```

```
a  passing.instance  4ce1a702a3a7, recorded 2026-10-18 06:54 UTC
   exit 0 after 180 ms, 2 calls, 2 values
b  failing.instance  01f75a212d4c, recorded 2026-10-18 07:12 UTC
   exit 1 after 940 ms, 3 calls, 2 values
calls  1 the same, 1 changed, 0 only in a, 1 only in b
speed  a 180 ms, b 940 ms (+760 ms, 5.22x)
       calls whose time changed most:
         +731 ms  GET https://api.example.com/v1/items (a call 1: 42 ms, b call 1: 773 ms)
codeinstances: the runs differ in 4 ways:
  - ended: a exit 0, b exit 1
  - stdout line 2: a "12 items", b "error: no items"
  - GET https://api.example.com/v1/items (a call 1, b call 1): response body at $.items: 12 items in a, 0 in b
  - GET https://api.example.com/v1/retry (b call 3) is only in b
```

Calls are paired by method, URL and request body, in order, and those left
over by method and URL alone, so a request whose body changed shows up as a
change instead of one call missing and one extra. For each pair, `diff`
compares the status and the response body: JSON bodies by member, down to the
first value that differs (numbers as written, so `1.0` and `1` differ), other
text by line, binary bodies by the first byte that differs. Headers
aren't compared, since they carry dates and other noise. How the runs ended
and what they printed are compared too.

Two separate runs read different clocks and draw different random numbers,
so by default `diff` compares only how many hooked values of each kind
(clock, random, bytes) each run asked for; `-values` compares the values
themselves. A different command,
kept environment or tool version is shown as a note, not counted as a
difference. Speed is shown but never counted: the whole run's time, and for
recordings made with 0.2.0 or later, the calls whose time changed most.

`diff` exits 0 when the runs are the same and 1 when they differ.

### Shrinking a failure

A failing run and a passing one usually differ in many answers: timestamps,
ids, counters, and somewhere among them the one that breaks the program.
`shrink` finds it:

```sh
codeinstances shrink -pass passing.instance failing.instance
```

```
codeinstances: 23 answers differ between the runs; the failure (exit 1) needs 1 of them (a is the passing run, b the failing one):
  - GET https://api.example.com/item/7 (failing call 11): status: a 200, b 500; response body at $.error: only in b ("out of service")
codeinstances: kept shrunk.instance (5b0e31c2d4a1: ./myprogram, exit 1, 23 calls, 3 values, 41 ms) after 11 runs
```

It pairs the calls of the two runs as `diff` does, then replays the program
again and again offline, each time with the passing run's answer swapped in
for some of the answers that differ. Delta debugging cuts the set down until
no single remaining answer can be swapped without the failure going away. A
run counts as the same failure when it ends the same way (the same exit code
or signal), and with `-match`, when its output also matches a regular
expression. The result is written to `shrunk.instance` (`-o` to change it):
the failing run with only the needed answers left in, and only the calls the
program made. It fails on replay with the same exit code or signal as the
original.

Only answers to calls both runs made can be swapped. Answers to calls only
the failing run made stay in as they were, and so do response headers and the
hooked clock and random values; `shrink` says how many such calls there are.

`shrink` refuses with exit 1 when the failing run doesn't fail again on
replay, or when the program also fails with the passing run's answers swapped
in, since then the answers that differ aren't the cause. A program that fails
only some of the time can lead it astray. `-max-runs` (200 by default, at
least 4) caps how many times the program runs, two of them held back for the
result, and `-timeout` (a minute by default) limits each run. Ctrl-C stops
the search. To shrink against the build that has the bug, give its command
after `--`; the shrunk file then keeps that command.

### Instances as regression tests

```sh
codeinstances test testdata/
```

```
ok    testdata/orders.instance  matched, 38 ms
FAIL  testdata/item7.instance  40 ms
        - ended exit 1, want exit 0 (recorded exit 1)
codeinstances: 1 of 2 tests passed
```

`test` replays every `.instance` file it's given, and every one under the
folders it's given. A run recorded with exit 0 has to replay exactly, so a
change that alters what the program sends or prints is caught. A run recorded
as a failure has to exit 0 now, against the same answers that once broke it:
the bug is fixed and stays fixed. `-expect match` or `-expect pass` sets one
rule for every file. Give the build under test after `--`. It exits 0 when
every test passed and 1 when any failed, so it drops into CI as it is.

A shrunk failure makes a good test: it holds only what the bug needs. A
failure found with `-match` that exits 0 is kept with exit 0, so test it with
`-expect pass`; `shrink` reminds you.

### Grouping failures

A nightly job that fails 40 times over a week is rarely 40 bugs. `group`
says how many it is:

```sh
codeinstances group failures/
```

```
codeinstances: 40 failing runs in 3 groups (212 runs passed)

1. 31 runs  fp1:6d1f0a93be27c4e1  2026-10-12 02:00 UTC to 2026-10-18 02:00 UTC
   ended  exit 1
   error  error: request <n> timed out after <duration>
   call   GET https://api.example.com/v1/items?page=* -> no answer
   - failures/run-0412.instance
   - failures/run-0413.instance
   - failures/run-0415.instance
   - failures/run-0416.instance
   - failures/run-0417.instance
   ... and 26 more (-v lists them)

2. 6 runs  fp1:1b77c0e9d2a4f815  2026-10-14 02:00 UTC to 2026-10-17 02:00 UTC
   ...
```

Each failing run (one that didn't exit 0) gets a fingerprint made of three
parts:

- how it ended;
- its error, read from stderr (from stdout when stderr has nothing): a Go
  panic or fatal error with the first three functions of the failing
  goroutine, outside the runtime and the testing package; a Python exception
  with the last three frames of its traceback; an error followed by `at`
  frames (Node.js, Java, C#) with its first three frames; the names of
  failed Go tests; or else the last line that looks like an error, skipping
  trailers such as `exit status 1`;
- the calls that got no answer or a 5xx status, by method and URL.

Before hashing, what changes between repeats is blanked out: times,
durations, UUIDs, hex ids (7 or more hex characters with a digit and a
letter), other long ids mixing letters and digits, numbers that don't follow
a letter, ports, and the values of query parameters. Status codes after
`status`, `code`, `errno` and `error:` are kept, since they are the failure.
Colour codes and other escape sequences are removed first. The id starts
with `fp1:`, the version of these rules, so ids made by different rules never
look alike. Runs with the same fingerprint are one group, biggest group
first, and the runs in a group are in the order they were recorded. Runs
that passed are counted and left out, and a file given twice counts once.
`show` prints a failing run's fingerprint too.

`group` takes files and folders like `test`; `-v` lists every run in a group
and `-json` prints the groups as JSON. It exits 0 whatever it finds.

### Sharing runs: the store, push, pull and report

Every instance already has a stable id: the SHA-256 of its canonical form,
which `verify` checks. The store uses it as the address.

```sh
CODEINSTANCES_TOKEN=a-long-random-token codeinstances serve -dir instances
```

`serve` runs a small HTTP server, on `127.0.0.1:7979` unless `-addr` says
otherwise, that keeps instances in a folder, each as `<id>.instance`. It
takes an upload only when the file is intact and its id checks out, and the
same file pushed twice is kept once. Every request needs
`Authorization: Bearer <token>`; `-open-read` lets anyone read, and uploads
still need the token. When no token is given, `serve` makes a random one and
prints it. Uploads have to be sent as `Content-Type: application/json`, which
a web page can't send to another site without asking first, and a store on a
loopback address answers only to loopback host names, so a page can't reach
it by pointing a name of its own at 127.0.0.1. The store reads its folder
when it starts and keeps an index in memory; each download is checked again
against its id. Its page lists the failures grouped by fingerprint and the
latest instances; a browser can open it only with `-open-read`, since it
can't send the token. The API is plain JSON:

| Request | What it does |
|---|---|
| `POST /v1/instances` | Store the instance in the body (201 new, 200 already there, 422 edited, 413 over `-max-size`, 503 when two uploads are already being read) |
| `GET /v1/instances` | List the instances, newest first (`?failed=true` for failing ones) |
| `GET /v1/instances/{id}` | Download one, by full id or a unique prefix of 12 or more hex digits |
| `GET /v1/groups` | The failing instances grouped by fingerprint |

```sh
export CODEINSTANCES_STORE=http://127.0.0.1:7979 CODEINSTANCES_TOKEN=a-long-random-token
codeinstances push failing.instance        # prints the id and the link
codeinstances pull 3f2a9c1e77b0            # writes 3f2a9c1e77b0.instance and checks it
codeinstances report failing.instance      # a Markdown bug report
```

`push` checks the store answers with the file's own id, and `pull` that the
file it gets is intact and is the one asked for. Neither follows a redirect,
so the token goes only where it was sent.
`report` writes a bug report to paste into an issue: the command, how the
run ended, the last lines of its output, the calls that failed, the
fingerprint, and the `pull`, `show` and `replay` commands that bring the run
back. Text from the run goes in code spans and blocks, so it can't mention
people or turn into links, and file names and commands are quoted for a
POSIX shell, so pasting them runs nothing else. [docs/ci.md](docs/ci.md) shows a GitHub Actions job that records the
tests and, when they fail, pushes the run and puts the report in the job
summary.

### Forking a run at a step

Replay runs a program against the whole recording. `fork` runs it against
the recording up to a step, then lets it carry on:

```sh
codeinstances fork -at 5 run.instance                       # calls 1-4 as recorded, live from call 5
codeinstances fork -at 5 -offline -answer 503.json run.instance  # what if call 5 had got a 503?
```

The requests that match the parent's calls before step 5 (by method, URL and
body, each call once) get the parent's answers. From there on, requests go
out to the real servers and are kept, as when recording; with `-offline`
nothing is sent, and a request the parent has no answer for gets a 502.
`-answer file.json` gives the request that matches the parent's call 5 a
made-up answer instead, such as `{"status": 503, "body": "busy"}` or
`{"error": "connection refused"}`. The parent's clock and random values come
back in order until they run out (`-fresh-values` hands out new ones). Give
another command after `--` to fork a changed build.

The result is a new instance, `fork.instance` by default (`-o`), that names
its parent and step, and marks each call: answered from the parent's call N,
given at the fork, or sent. `fork` exits as the program did (so a program
that exits 2 looks like a usage error) and says how many calls went each
way, and whether the given answer was never asked for or the program left
some of the parent's earlier answers unused, which means it changed before
the step.

A fork runs the command stored in the parent with the parent's kept
environment, and without `-offline` it reaches the real servers. Treat a
parent from someone else as you would a script from someone else: look at it
with `show` first, or give your own command after `--`. `fork` has no `-env`;
it takes the parent's. A run recorded with `-resolve` or `-upstream-ca` needs
them again for a live fork. The fork replays, compares and shrinks like any other
instance; `codeinstances diff run.instance fork.instance` shows what the
step changed. Forking one past the last call (`-at` with the call count plus
one) answers every recorded call and sends only what the program asks for
beyond them.

### Signing runs

The id shows a file was changed, not who made it: anyone can edit a run and
write a new id. A signature shows who kept it.

```sh
codeinstances keygen -o ci                        # ci.key (secret) and ci.pub
codeinstances record -sign ci.key -signer ci -- go test ./...
codeinstances sign -key ci.key run.instance       # or sign a run afterwards
codeinstances verify -trust ci.pub run.instance   # exit 0 only if ci.key signed it
```

`keygen` writes an Ed25519 key pair: `ci.key`, an unencrypted PEM private
key readable only by you (except on Windows, where the check isn't made,
signing refuses a key file other users can read or change, as ssh does), and
`ci.pub`, one line to give to whoever checks your runs. A trust file is
those lines put together. `keygen` never overwrites a key. It writes into
the folder you're in, so keep `*.key` out of version control.

A signature vouches for the run's content id, the id the file had before
it was signed, so several keys can sign one run (a CI runner, then the
person who reviewed it), and signing again with a key replaces its own
signature. The file's id covers the signatures too, so signing gives the
file a new id; `sign` prints both. Every command that reads a file checks
its signatures, and refuses a file whose signature doesn't check out, as it
refuses one whose id doesn't. `verify` lists who signed; with `-trust` it
exits 1 unless a key in the trust file did. `show` and `report` list the
signatures too.

A store started with `serve -trust keys` takes only uploads signed by one of
those keys (runs already in its folder are served as they are), and `pull -trust keys` keeps a run only if one of them signed it.
`record` and `fork` sign as they keep the run with `-sign key` (and
`-signer name`). `shrink` doesn't carry signatures over, since the shrunk run
is a run of its own; sign it if you want it signed. A fork's `parent` is the
id of the file it was forked from, signatures and all, so a signed fork
vouches for the parent it names; signing the parent afterwards gives that
file a new id, which the fork doesn't know.

`record -sign` runs the program as you, so the program can read the key
file. When the program shouldn't be able to sign (an AI agent, tests that
run code you don't control), record without `-sign` and sign afterwards, in
a step the program has no part in. Until it's signed, the file is only as
safe as the place it sits: whoever can change it there can change what gets
signed. [docs/ci.md](docs/ci.md) shows that in CI.

### Running instances on demand

A store can run what it keeps. Start it with `-run`, and anyone with the
store's token can have it replay a run, or fork one at a step, and get back
how it went:

```sh
codeinstances serve -dir instances -token-file token -run -run-trust runners.pub
codeinstances run 3f2a9c1e77b0                             # replay it on the store
codeinstances run -at 4 -o what-if.instance 3f2a9c1e77b0   # fork it there; keep and download the fork
```

The store runs a kept instance the way a function service runs a function
on a request: the run is the function, and a request runs it. A replay
answers whether the program, as installed where the store runs, still does
what the run did. A fork tries the run again from a step without anyone
setting the program up on their own machine. Every run is answered from the
recording: HTTP and HTTPS requests sent through the proxy settings never go
out, and a fork gets a 502 for anything the parent has no answer for.

Running commands on request needs care, so the rules are tight:

- `-run` needs `-run-trust`: the store runs only instances signed by one of
  those keys, checked at every run, whatever is in its folder. Keep that
  list apart from `-trust`, which says whose uploads the store takes. A
  signature says who kept a run; a run key says its runs are fit to run on
  this machine.
- It runs the command each instance recorded, and nothing a request says
  can change it. A request picks the run and, for a fork, the step, whether
  to hand out fresh values and whether to keep the fork.
- A fork request can give an answer at the step (`run -answer file.json`)
  only when the store was started with `-run-answers`. That answer is input
  the program acts on: a run that downloads a script and runs it would run
  whatever it was given. Turn it on only for runs you'd let anyone with the
  token feed data to.
- The store keeps a fork only when asked (`run -keep`, or `-o` to download
  it), up to `-max-kept` forks (1,000 by default) and `-max-size` each.
  With `-sign key` it signs them as `-signer name`: that signature says "ran
  on this store, with these answers". The store refuses to start when that
  key is in `-trust` or `-run-trust`, and never runs a fork only its own key
  signed.
- Runs start from a minimal environment (`PATH`, `HOME`, the locale, the
  temporary folder and the like, plus what the instance kept), not the
  store's, in a fresh empty folder each (or `-run-dir`). On Linux the store
  makes itself unreadable to the programs it runs, so they can't read its
  environment through `/proc`, unless it runs as root, which can read
  anything; never run it as root. Give the token with `CODEINSTANCES_TOKEN` or
  `-token-file`: a `-token` shows in the process list.
- One run at a time by default (`-max-runs`), a minute each
  (`-run-timeout`). More runs at once get 503. On Linux and macOS each run
  has a process group of its own, and the whole group is stopped when the
  run ends, times out or its client goes away.
- The answer carries how the run ended and the last 64 KiB of each output
  stream. `run` prints them indented, so nothing the program prints reads as
  a line from codeinstances, and exits as `replay` does, or with `-at` as the
  program did.

The program has to be where the store runs, at the path it was recorded
with, as for a local replay. And the store runs signed code as the user it
runs as: a program can read whatever that user can, the store's folder,
token file and key file included. Give the store a user of its own, or a
container, and keep its key and token files where the programs can't read
them.

### Commands

| Command | What it does |
|---|---|
| `record [flags] -- command [args...]` | Run the command and keep the run (`-o` names the file, `run.instance` by default) |
| `replay [flags] file [-- command [args...]]` | Run it again against the recording and compare |
| `diff [flags] a b` | Compare two instances: how they ended, their output, calls, values and speed |
| `shrink [flags] -pass passing failing [-- command]` | Find the fewest failing answers that keep the failure, and keep that instance |
| `test [flags] file-or-dir... [-- command]` | Replay instances as regression tests |
| `group [flags] file-or-dir...` | Fingerprint failing runs and group the ones that fail the same way |
| `serve [flags]` | Run a store that keeps instances under their ids |
| `push [-to url] file-or-dir...` | Upload instances to a store |
| `pull [-from url] [-o file] id` | Download an instance by id or unique prefix |
| `report [-url store] file` | Write a Markdown bug report for an instance |
| `run [-on url] [-at N [-answer file] [-o file]] id` | Have a store started with `-run` replay a run it keeps, or fork it at a step |
| `fork [flags] -at N parent [-- command]` | Run on from a step: the parent's answers before it, live (or given) from it |
| `keygen [-o name] [-label text]` | Make a key pair for signing: `name.key` and `name.pub` |
| `sign -key file.key [-signer name] [-o out] file` | Sign an instance (in place unless `-o`) |
| `show [-json] file` | Describe an instance |
| `verify [-trust keys] file...` | Check files haven't been changed since they were recorded or signed, and with `-trust` that a trusted key signed them (exit 1 if not) |
| `version [-json]` | The version, the format version and the commit it was built from |

`record` flags: `-env NAME` keeps an environment variable and sets it again on
replay; `-resolve host=addr` sends a host name to another address, like curl's
`--resolve`; `-upstream-ca file.pem` trusts extra certificates when reaching
real servers; `-keep-secrets` keeps the headers that are redacted by default;
`-timeout 30s` kills the program after that long; `-q` hides its output;
`-sign file.key` signs the instance as it's kept, as `-signer name`.

`diff` flags, which go before the two files: `-values` compares the hooked
values themselves; `-ignore-stderr` leaves stderr out; `-json` prints the
comparison as JSON.

`shrink` flags: `-pass file` (required) is the passing run; `-o` names the
file to write; `-match regexp` must also be found in the output; `-max-runs`
and `-timeout` as above; `-q` hides the line per run; `-json` prints the result
as JSON.

`test` flags: `-expect auto|match|pass`; `-ignore-stderr`; `-timeout` per
test (a minute by default); `-v` lists the notes of passing tests too;
`-json` prints the results as JSON.

`group` flags: `-v` lists every run of each group; `-json`.

`serve` flags: `-dir` (default `instances`), `-addr` (default
`127.0.0.1:7979`), `-token` (default `$CODEINSTANCES_TOKEN`), `-open-read`,
`-max-size` in bytes (default 64 MiB), `-host name` to answer to another host
name too, such as the public name of an HTTPS proxy in front of a loopback
store (repeatable), and `-trust keys` to take only runs signed by those
keys (repeatable); `-token-file`; `-run` to run instances on demand, with
`-run-trust keys` (required, repeatable), `-run-answers`, `-run-dir`,
`-run-timeout` (default 1m), `-max-runs` (default 1), `-max-kept` (default
1,000), and `-sign key` and `-signer name` to sign the forks it keeps. `push` and `pull` take `-to` or `-from` (default
`$CODEINSTANCES_STORE`) and `-token`; `push -json` prints the results as JSON;
`pull -o` names the file and `pull -max-size` (default 64 MiB) is the largest
file it takes; `pull -trust keys` keeps the run only if one of those keys
signed it. `report -url` (default
`$CODEINSTANCES_STORE`) adds the `pull` command; `-lines` sets how much output
to show.

`run` flags: `-on url` (default `$CODEINSTANCES_STORE`) and `-token`;
`-strict` and `-ignore-stderr` as for `replay`; `-at N` to fork instead of
replay, with `-answer file.json`, `-fresh-values`, `-keep` to have the store
keep the fork and `-o file` to keep and download it; `-json` prints the
store's answer as JSON.

`fork` flags: `-at N` (required); `-o` (default `fork.instance`);
`-answer file.json`; `-offline`; `-fresh-values`; and `-resolve`,
`-upstream-ca`, `-keep-secrets`, `-timeout`, `-q`, `-sign` and `-signer` as
for `record`.

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
`codeinstances verify` notices any edit, and can carry Ed25519 signatures of
its content id, so it shows who vouched for it.

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
and checks `verify` catches an edited file. Then it compares the instance
with a second run, recorded while the servers were up, of the program that
makes one extra request, and checks `diff` finds that request and times every
call the two runs share. It records a third run while the HTTPS server is
breaking item 7, and checks that `shrink`, against the first run, needs only
that one answer of all the ones that differ. Then it puts the first run and
the shrunk failure in a folder and checks that `test` fails the program as it
is and passes a fixed build that carries on when an item can't be read. It
records two more failing runs, the item 7 bug again and then a second bug
where the service refuses orders, and checks that `group` puts the three
failing runs in two groups and leaves the passing one out. Last, it starts a
store with a token on a loopback port, pushes the three failing runs, pulls
one back by a 12-digit id prefix and compares the bytes, checks the store's
grouping, and checks `report` says how to pull and replay the run. With the
servers gone, it forks the first run offline at its order call with a 503
given as the answer, and checks the program reports the refused order; then
it forks one past the last call and checks `diff` finds the fork the same as
its parent. Last, it makes a key, signs the refused-order run and checks
`verify -trust` takes it and refuses the unsigned run and a key it wasn't
given; edits the signed run's output, writes a matching id, and checks
`verify` still catches it on the signature; and starts a store with the key
as trusted and checks it refuses the unsigned run and takes the signed one.
With that store running instances on demand, it checks `run` replays the
signed run there and it matches; that a what-if fork on the store, with the
order accepted, gets the program as far as the status call the run never
made, and is kept signed by the store's own key; and that the store won't
run the unsigned run.
Progress goes to stderr and a JSON
report to stdout; any failed check makes it exit 1.

`sh scripts/record-tests.sh` runs every test, the race detector, the demo and
the release smoke check, and writes them to [test/results](test/results). From
the latest recording, on a 2-CPU Linux machine (details in the files):

- 110 Go tests pass, and the race detector finds nothing
  ([tests.txt](test/results/tests.txt), [race.txt](test/results/race.txt)).
- The demo's 30 checks pass. Recording the 23 calls took 83 ms; the 10
  offline replays took 37 to 47 ms each; the instance file was 20,625 bytes;
  comparing it with the second run took 12 ms. In this recording, 22 answers
  differed between the first run and the failing one, and `shrink` found the
  1 that matters in 11 runs and 383 ms; the two regression tests ran in 78 ms;
  `group` put the three failing runs in 2 groups in 11 ms; pushing the three
  to a store took 16 ms and pulling one back 4 ms; the offline what-if fork
  took 20 ms; signing a run took 6 ms and checking it against a trusted key
  3 ms
  ([demo.json](test/results/demo.json)).
- The smoke check passes on release-style binaries, including recording,
  replaying, comparing, testing and signing runs of a Python program that
  knows nothing of CodeInstances
  ([smoke.txt](test/results/smoke.txt)).

The tests cover the format (round trips, the id catching edits, a published
test vector for the id, files with repeated or misspelled members refused,
atomic saves),
the proxy (HTTPS and HTTP replayed with the servers shut down, 64 concurrent
requests replayed three times, strict order, 4 and 8 MiB bodies, redaction,
upstream errors replayed as the same 502), changed programs (an extra request,
a missing request, an extra random value, an unhooked clock), comparing runs
(call pairing, changed request bodies, JSON paths, line and byte differences,
value counts, notes that don't count, speed order), shrinking (two answers
out of five found, refusals, the run limit, the shrunk file failing on
replay), regression tests (match and pass, a fixed build), fingerprints
(normalising, Go panics and fatal errors, go test output, Python, Node.js
and Java traces, escape codes, failed calls, grouping), the store (ids and
prefixes, edited and oversized uploads, tokens, the page escaping what runs
print, a lying server caught by pull, trusted keys), runs on demand (replay
and fork on the store, forks kept only when asked and capped, given answers
only when allowed, a fork only the store signed never run, the minimal
environment, a grandchild process stopped with its run, one run at a time,
the time limit, refused requests, a lying store caught by the client), signatures (edits with
a fresh id, signatures moved to another run, malformed signatures, two
signers, key files and trust files), bug reports, crashes,
killed
programs and timeouts, a recorder killed with SIGKILL leaving no file, and
Ctrl-C reaching the program while the recorder finishes.

## Limits in v0.8.0

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
- Only header values are redacted. Secrets in bodies, in URL query strings
  (an `api_key=` parameter, say) and in the command line are kept as they
  are, and so are bytes from `hook.Read`, which may be keys.
- Replay hands the program the redacted values, so a program that prints a
  cookie it was sent will diverge.
- Each process that uses the hook package replays the recorded values from
  the start, so values are recorded and replayed exactly for one hooked
  process at a time.
- Output is kept up to 16 MiB per stream.
- `diff` compares a call's status and body, not its headers.
- `diff` doesn't report cost (money, tokens or data sent); it reports
  differences and time.
- `shrink` needs a passing run to take answers from, and it only swaps whole
  answers to calls both runs made; it doesn't shorten a response body, drop
  requests, or change headers or hooked values.
- `shrink` assumes the program fails the same way every time it gets the same
  answers. One that fails at random can mislead it.
- A fingerprint is a heuristic. Two bugs that end the same way, print the
  same error line and fail the same calls share one; one bug whose message
  carries a word that changes (a file name, say) splits into several groups.
  Calls are told apart by method and URL, not body, so every failing call to
  one GraphQL or JSON-RPC endpoint looks the same. Python exception groups
  and stack traces in other formats aren't read as traces.
- The store is one server and one folder, with one token and no accounts.
  It reads every file when it starts and keeps an index in memory, which
  suits thousands of instances, not millions; files put in the folder by
  hand are seen at the next start. Reading an upload takes many times its
  size in memory, so it reads two at a time and takes up to 64 MiB each by
  default. It serves plain HTTP; put it behind an HTTPS proxy to reach it
  over a network.
- Nothing is scrubbed before a push or in a report beyond the redacted
  headers: changing bodies or query strings would stop the run replaying.
- The parent's answers come back with their secret headers redacted, as in
  a replay, so a program that reads a cookie from them gets `[redacted]`,
  and from the step on sends it to the real servers that way. Record the
  parent with `-keep-secrets` to fork through a login.
- A fork answers by matching requests to the parent's calls, so a program
  that changed what it sends before the step goes live (or offline) earlier
  than the step. Clock and random values carry no time in the file, so a
  fork hands back the parent's values in order rather than only those before
  the step.
- A signature shows that the key's holder signed the content, nothing more.
  The signer name and time are their claims, not checked facts: there's no
  timestamp authority, and a clock can be set to anything.
- Keys are yours to manage. There's no revocation, no expiry and no key
  directory; a trust file is the list, and a key leaves it when you take its
  line out. Anyone who gets a private key can sign as its owner.
- A signature can be taken off a file and a new id written, leaving a
  valid unsigned file. Only a reader that requires a trusted signature
  (`verify -trust`, `serve -trust`, `pull -trust`) notices.
- Signing vouches for what was kept, not that it was kept faithfully: a
  machine that records can sign a run it changed first. Sign where you
  trust the recording, as close to it as you can.
- A program recorded with `record -sign` runs as you and can read the key.
  Sign afterwards, in a separate step, when the program mustn't hold it.
- A signature fixes a run's content id, not the file's id: anyone can take
  a signature off, or add an entry in an algorithm this version doesn't
  check, and get a new file id. Name a signed run by its content id.
- The key file isn't encrypted, and on Windows its permissions aren't
  checked.
- Only Ed25519 is made and checked. Signatures in other algorithms are kept
  and left unchecked.
- A store that runs instances runs signed code as its own user, and a
  program can read what that user can. Only on Linux, and not as root, does
  the store hide its own environment from them. Run it as a user of its own
  or in a container.
- Runs are kept off the network only for HTTP and HTTPS sent through the
  proxy settings. A program that ignores them, opens its own sockets or
  looks names up in DNS reaches the network from the store's machine; a
  container without a network stops that.
- On Windows, a run that times out is stopped, but processes it started
  aren't.
- A program that isn't installed where the store runs, at the path it was
  recorded with, can't run there.
- A run's answer comes at the end, not as the program prints, and carries
  only the last 64 KiB of each output stream.
- Replay has no time limit unless you give `-timeout`, so give one when
  replaying a run that timed out.
- The recording proxy listens on a loopback port with no password while the
  program runs, so other programs on the same machine could send traffic
  through it.

## Where it goes next

CodeInstances treats each run as an object you can do things to. This release
does ten of them: keep, replay, compare, shrink, test, group, share, fork,
sign and run on demand. That's every item on the plan; what comes next is
decided as it's used.

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
