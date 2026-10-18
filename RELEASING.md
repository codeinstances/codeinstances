# Releasing CodeInstances

Releases go out on their own. Nobody clicks Publish. This file is the record of
how that works, for whoever (person or Claude session) works on this repo next.

## The short version

1. Raise `const Version = "X.Y.Z"` in `codeinstances.go`.
2. Write what changed into the commit message.
3. Push the commit to `main`.

When the `test` workflow passes on that commit (the full suite on Linux and macOS,
and the smoke check on Windows binaries on a Windows runner), the `release` workflow builds the
binaries, smoke-tests them, then tags the commit `vX.Y.Z` and publishes the
release "CodeInstances vX.Y.Z" with the archives and SHA256SUMS attached. If any build
or check fails, nothing is tagged and nothing goes public. Fix it, push again,
and the next green run picks it up.

Pushing without raising the version is fine: the release workflow sees the tag
already exists and stops quietly.

## What it does, step by step

[.github/workflows/release.yml](.github/workflows/release.yml) starts when a
`test` run finishes; the push alone doesn't start it.

- **plan.** Goes on only if the `test` run passed, came from a push to `main` in
  this repo, and `main` still points at that commit (if `main` moved on, the run
  for the newer commit decides). Reads `Version` from `codeinstances.go`. Stops
  quietly if `vX.Y.Z` is already a tag. Fails loudly if the version is lower than
  the newest tag. [scripts/release-plan.py](scripts/release-plan.py) has the
  rules, and [scripts/release_plan_test.py](scripts/release_plan_test.py) its cases.
- **linux.** Builds `codeinstances` and `codeinstances-demo` from that exact
  commit, pure Go with no cgo so they are static, on x86-64 and ARM runners,
  checks they are static, and runs [scripts/smoke.py](scripts/smoke.py) on them.
- **macos.** Builds for Apple silicon and Intel. The Apple silicon binaries run
  the smoke check; the Intel ones are checked for their architecture.
- **windows.** Builds the `.exe` files on Linux.
- **windows-check.** Runs the smoke check on the Windows binaries on a Windows
  runner.
- **release.** Runs only after every build and check passed. Writes SHA256SUMS and
  the notes, then creates the tag and the release in one step with
  `gh release create --target <commit>`, on the repo's own `GITHUB_TOKEN`
  (`contents: write` on this job only). A `concurrency` group per tag means two
  runs for the same version can't publish twice.

The smoke check, on each binary set: both binaries report the version in
`codeinstances.go` and format `codeinstances/0`; `codeinstances-demo` passes
every check (a Go program recorded over HTTPS and HTTP with hooked values,
replayed three times with the servers stopped, an extra request and an
unhooked clock caught, an edited file caught); and a Python program is
recorded against a live local server, the server stopped, the run replayed,
and `show` and `verify` checked on the file.

## Files that go out

| System | File |
|---|---|
| Linux, x86-64 (static) | codeinstances_linux_amd64.tar.gz |
| Linux, ARM64 (static) | codeinstances_linux_arm64.tar.gz |
| macOS, Apple silicon | codeinstances_darwin_arm64.tar.gz |
| macOS, Intel | codeinstances_darwin_amd64.tar.gz |
| Windows, x86-64 | codeinstances_windows_amd64.zip |
| Checksums | SHA256SUMS |

Each archive holds the two binaries, LICENSE, NOTICE, THIRD_PARTY_LICENSES.md
and README.md. Archive names carry no version on purpose: links like
`https://github.com/codeinstances/codeinstances/releases/latest/download/codeinstances_linux_amd64.tar.gz`
keep working from one release to the next, so the README and the site never need
editing for a release.

## After a release

A Claude session can't create tags or releases itself, which is why the workflow
does it on the repository's token. After pushing a raised version, check:

1. The `test` run and then the `release` run finished green
   (`gh run list --repo codeinstances/codeinstances`).
2. The release exists with all six files
   (`gh release view vX.Y.Z --repo codeinstances/codeinstances`).
3. The `latest/download` links resolve, and a downloaded archive's SHA256 matches
   SHA256SUMS.
4. A downloaded Linux binary runs: `codeinstances version` names the release.

## By hand

- A release published by hand gets the binaries attached by the same workflow.
- To rebuild the files of an existing tag, run the `release` workflow by hand
  with the tag (`gh workflow run release --repo codeinstances/codeinstances -f tag=vX.Y.Z`).
