# The instance format, version 0

An instance is one run of a program, kept as a file. This page describes the
file so that any tool can read or write one. The format string for this
version is `codeinstances/0`.

[example.instance](example.instance) is a real one: a Python one-liner that
fetched a small JSON file over HTTP.

## The file

One JSON object, UTF-8. The suggested extension is `.instance`. Readers must
ignore members they don't know, and must refuse a file whose `format` isn't a
version they read.

| Member | Type | What it holds |
|---|---|---|
| `format` | string | Always `"codeinstances/0"` for this version |
| `id` | string | `"sha256:"` and 64 hex digits; see [The id](#the-id) |
| `created` | string | When the recording started, RFC 3339, UTC |
| `tool` | string | What wrote the file, such as `"codeinstances 0.1.0"` |
| `command` | object | What was run; see below |
| `result` | object | How it ended and what it printed |
| `calls` | array | Every exchange with the outside world, in arrival order |
| `values` | array | Every clock or random value the program asked for |

### command

| Member | Type | |
|---|---|---|
| `argv` | array of strings | The program and its arguments. Never empty |
| `dir` | string, optional | The working directory at recording time, for the record only |
| `env` | object, optional | Environment variables the recording was told to keep, name to value. Replay sets them again |

The rest of the environment is not kept, because it often holds secrets.

### result

| Member | Type | |
|---|---|---|
| `exit_code` | integer | The exit code; -1 when the program was killed by a signal |
| `signal` | string, optional | The signal that killed it, such as `"killed"` |
| `timed_out` | boolean, optional | The recorder killed it at its time limit |
| `duration_ms` | integer | Wall time from start to exit |
| `stdout`, `stderr` | body, optional | What it printed. Absent when nothing was printed |
| `stdout_truncated`, `stderr_truncated` | boolean, optional | Only the first part was kept (codeinstances keeps 16 MiB of each) |

### calls

Each element is one exchange. Version 0 has one kind, `"http"`.

| Member | Type | |
|---|---|---|
| `seq` | integer | 1, 2, 3 and so on, in the order requests arrived |
| `kind` | string | `"http"` |
| `at_ms` | integer | When the request arrived, in milliseconds since the run started |
| `request` | object | `method`, `url`, optional `header` and `body` |
| `response` | object | `status`, optional `header` and `body` |
| `error` | string | Set instead of `response` when no answer came back |

Exactly one of `response` and `error` is present.

`url` is absolute: scheme, lowercase host, the port only when it isn't the
scheme's default, path and query, as in `https://api.example.com/v1/items?page=2`.
Headers are objects from header name to a list of values, as sent or received,
minus the headers that belong to one connection (`Connection`, `Keep-Alive`,
`Transfer-Encoding` and the like).

By default the values of `Authorization`, `Proxy-Authorization`, `Cookie`,
`Set-Cookie`, `X-Api-Key`, `Api-Key`, `X-Auth-Token` and `X-Amz-Security-Token`
are replaced with `"[redacted]"`. Bodies are kept as they were.

### values

| Member | Type | |
|---|---|---|
| `seq` | integer | 1, 2, 3 and so on, in the order they were handed out |
| `kind` | string | `"clock"`, `"random"` or `"bytes"` |
| `value` | string | A clock value is a time in RFC 3339 with nanoseconds; a random value an unsigned 64-bit integer in decimal; bytes are base64 |

### body

An object with exactly one member: `text` when the bytes are valid UTF-8,
otherwise `base64` (standard alphabet, with padding). An empty body is left
out rather than written as an empty object.

## The id

The id lets anyone check that a file is the one that was recorded. It is
`"sha256:"` followed by the lowercase hex SHA-256 of the file's canonical
form, which is:

1. the JSON object with its `id` member removed,
2. object members sorted by name (by UTF-8 bytes), at every level,
3. no whitespace between tokens,
4. numbers exactly as written in the file,
5. strings escaped minimally: `\"`, `\\`, the control characters U+0000 to
   U+001F (as `\n`, `\r`, `\t` or `\u00XX`), and U+2028 and U+2029 as `\u2028`
   and `\u2029`; everything else as it is.

Whitespace and member order in the file don't change the id; any change to a
value or an added member does. `codeinstances verify` checks it.

The id shows a file was changed. It doesn't show who made it: anyone can
change a file and write a new id. Signed instances are on the plan for a later
version.

## How replay uses a file

This part describes codeinstances, not the format, but other tools that
replay instances should behave the same way.

- A request is matched to a recorded call by method, URL and the SHA-256 of
  its body. Headers are not compared.
- Several calls with the same match are answered in recorded order.
- By default requests may arrive in any order, since threads race. A strict
  replay requires the recorded order exactly.
- A call recorded with an `error` is answered with a 502 and the same body the
  program got when it was recorded.
- A request with no recorded match gets a 502 and counts as a divergence, and
  so does every recorded call that was never made.
- Values are handed back in recorded order, separately for each kind. A
  program that asks for more than were recorded gets fresh ones, and that
  counts as a divergence; so do recorded values left unused.

## Changes

Version 0 is the first. A version that changes the meaning of existing
members gets a new format string; members added within a version must be
safe for older readers to ignore.
