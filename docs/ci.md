# CodeInstances in CI

A failing CI run is usually gone by the time anyone looks at it. With
CodeInstances the failing run is kept, sent to your store, and summed up in
the job's summary, so it can be pulled and replayed on a laptop.

This page shows a GitHub Actions job. The same three steps work in any CI:
record the test run, and when it fails, push the instance and print a report.

## The store

Run a store somewhere your CI can reach, with a token:

```sh
CODEINSTANCES_TOKEN=a-long-random-token codeinstances serve -dir /srv/instances -host instances.example.com
```

It listens on `127.0.0.1:7979`; put your usual HTTPS proxy in front of it,
passing the public host name along. `-host` lets the store answer to that
name, since a store on a loopback address answers only to loopback names
otherwise. Don't open the store's own port to the network: it speaks plain
HTTP, token included. In the repository's settings, add a
variable `CODEINSTANCES_STORE` (the store's address, such as
`https://instances.example.com`) and a secret `CODEINSTANCES_TOKEN`. The
address is a variable, not a secret, so it shows in the job summary's `pull`
command instead of being masked.

## The workflow

```yaml
name: test
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: stable

      - name: Install codeinstances
        run: |
          mkdir -p "$HOME/bin"
          curl -fsSL https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_linux_amd64.tar.gz \
            | tar -xz -C "$HOME/bin"
          echo "$HOME/bin" >> "$GITHUB_PATH"

      # Fetch modules first, so the recording holds the tests' own traffic.
      - run: go mod download

      - name: Test, keeping the run
        run: codeinstances record -q -o run.instance -- go test ./...

      # The report comes first, so it's written even when the store can't
      # be reached (or, on a pull request from a fork, there's no token).
      - name: Report the failing run
        if: failure()
        env:
          CODEINSTANCES_STORE: ${{ vars.CODEINSTANCES_STORE }}
        run: codeinstances report run.instance >> "$GITHUB_STEP_SUMMARY"

      - name: Push the failing run
        if: failure()
        env:
          CODEINSTANCES_STORE: ${{ vars.CODEINSTANCES_STORE }}
          CODEINSTANCES_TOKEN: ${{ secrets.CODEINSTANCES_TOKEN }}
        run: codeinstances push run.instance

      # Without a store, keep the file with the run instead.
      - uses: actions/upload-artifact@v4
        if: failure()
        with:
          name: failing-run
          path: run.instance
```

`codeinstances record` exits with the program's exit code, so the test step
fails the job as it always did. The report in the job summary names the
command, the error and the failed calls, and gives the `pull` and `replay`
commands that bring the run back.

On a laptop:

```sh
codeinstances pull -from https://instances.example.com 3f2a9c1e77b0
codeinstances replay 3f2a9c1e77b0.instance
```

Over a week, `codeinstances group` on the store's folder, or the store's own
page, shows how many different failures the job really had.

## Signing the runs

To show a run came from your CI and wasn't changed on the way, make a key
once, on your own machine:

```sh
codeinstances keygen -o ci -label ci@example.com
```

Add the contents of `ci.key` as a secret `CODEINSTANCES_SIGNING_KEY`, and
keep `ci.pub` with whoever checks the runs. Record as before, without the
key, and sign in a step of its own, right after the test step and before the
report and push:

```yaml
      - name: Sign the run
        if: always()
        env:
          SIGNING_KEY: ${{ secrets.CODEINSTANCES_SIGNING_KEY }}
        run: |
          if [ -n "$SIGNING_KEY" ] && [ -f run.instance ]; then
            key="$RUNNER_TEMP/ci.key"
            (umask 077 && printf '%s\n' "$SIGNING_KEY" > "$key")
            codeinstances sign -key "$key" -signer ci run.instance
            rm -f "$key"
          fi
```

The key is never in the test step's environment or on disk while the tests
run, so the tests, their dependencies or an agent being recorded can't sign
with it. `codeinstances` won't sign with a key file others can read, hence
the `umask`. A pull request from a fork gets no secrets, so its run is kept
unsigned.

A store started with `-trust ci.pub` takes only signed runs, and on a laptop
`codeinstances pull -trust ci.pub ...` or `codeinstances verify -trust ci.pub
run.instance` checks the key before you trust what the run says.

## Running failures on the store

On a machine that has the project built as CI builds it, a store started
with `-run` replays signed runs on request, so a failing run can be tried
again, or tried with another answer at a step, without anyone setting the
project up on a laptop:

```sh
codeinstances serve -dir /srv/instances -host instances.example.com -trust ci.pub -run -run-dir /srv/checkout
codeinstances run 3f2a9c1e77b0                     # from anywhere with the token
```

The store runs each instance's recorded command, offline, as the user it
runs as, and only instances `ci.pub` signed. Give it a user of its own, and
see the README for the rest of the rules.

## Things to know

- This page is an example. The project's own CI doesn't run it.
- Go's HTTP client doesn't use a proxy for `localhost` or `127.0.0.1`, so
  tests that talk to a server they start themselves aren't recorded; their
  traffic to real services is. See the README's limits.
- An instance keeps request and response bodies, URL query strings and the
  command line as they were. Keep the store as private as your CI logs.
- A signature shows the run was signed by whoever holds the key. Anyone who
  can change the workflow can use the secret too, so it vouches for your CI
  as far as you trust the people who can edit it. Steps in one job share a
  machine, so a test step that leaves a process running could still reach
  the signing step; for more, sign in a separate job.
- On a public repository, the job summary is public too: the report shows
  the last lines of the output and the URLs of failed calls, query strings
  included. Leave the report step out there, or keep the summary to the id.
