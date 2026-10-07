# BHT/1 in Go

An independent Go implementation of **BHT/1**, a binary HTTP wire protocol. The
project provides a static file server (`bserve`), a command-line client
(`bcurl`), and a sans-I/O codec that can be tested without sockets.

The protocol specification and conformance vectors live in [`spec/`](spec/).
Sample files for the server are in [`www/`](www/).

**Requirements:** Go 1.25 or later. Standard library only. No `net/http`.

## Quick start

Build:

```powershell
go build -o bin/ ./cmd/...
```

Run the server:

```powershell
.\bin\bserve.exe .\www 9000
```

Fetch a file (one TCP connection, even for multiple URLs):

```powershell
.\bin\bcurl.exe localhost:9000/index.html
.\bin\bcurl.exe localhost:9000/hello.txt localhost:9000/docs/nested.txt
```

Verbose mode prints an annotated hexdump of every frame:

```powershell
.\bin\bcurl.exe -v localhost:9000/hello.txt
```

## Commands

### bserve

```
bserve ROOT [PORT] [--host 127.0.0.1] [--idle-timeout 30] [-q | --debug]
```

Serves files from `ROOT` over BHT/1. Keeps the connection open after each
response. Returns 404 for missing files, 400 for malformed requests, 405 for
unsupported methods.

### bcurl

```
bcurl [-v] [-I] [-o FILE] [-H name:value]... [--max-data N] [--timeout S] URL...
```

Fetches one or more URLs over a single connection. Bodies go to stdout unless
`-o` is set. With `-v`, frame dumps go to stderr.

| Exit code | Meaning |
|---|---|
| 0 | success |
| 2 | usage error |
| 4 | HTTP 4xx |
| 5 | HTTP 5xx |
| 7 | connection failed |
| 8 | protocol error |

## Project structure

```
cmd/bserve/          server entry point
cmd/bcurl/           client entry point
internal/wire/       BHT/1 codec (frames, headers, messages, streams)
internal/hexdump/    field-by-field frame annotator for bcurl -v
internal/server/     TCP accept loop, file resolution, connection handling
internal/client/     pipelined fetch over one connection
interop/             optional cross-language tests (see below)
spec/SPEC.md         protocol specification
spec/vectors/        hand-written conformance vectors (JSONL)
www/                 sample document root
```

The codec has no network imports. Socket code is a thin wrapper around
`wire.Reader` and `wire.Assembler`, which makes incremental parsing and
one-byte-at-a-time tests straightforward.

## Testing

Run all tests:

```powershell
go vet ./...
go test ./...
```

This runs 70 tests and 220 subtests. Interop tests against a Python BHT/1
implementation are skipped unless you set:

```powershell
$env:BHT1_PY_IMPL = "path\to\directory\with\bserve.py"
go test ./interop/ -v
```

Optional fuzzing:

```powershell
go test ./internal/wire -run X -fuzz FuzzChunkingIsInvisible -fuzztime 1m
```

## Design summary

- **Self-delimiting frames.** Every message boundary comes from declared
  lengths and the `END_MESSAGE` flag, not from closing the connection.
- **Stream vs connection errors.** Recoverable failures produce a RESPONSE on
  that stream; fatal framing errors send ERROR on stream 0 and close.
- **Unknown frame types are skipped** (forward compatibility).
- **Static header table** for the ten most common header names; other names are
  length-prefixed literals.
- **Path safety:** dot segments, backslashes, control bytes, device names, and
  paths outside the document root are rejected.
- **Large files are streamed** in DATA frames without loading the whole file into
  memory.

Further analysis of the specification, including ambiguities found during
implementation, is in [SPEC-NOTES.md](SPEC-NOTES.md).

## What to commit

Push source, spec, vectors, sample site, and documentation. Do **not** push
build output or local artifacts:

| Push | Do not push |
|---|---|
| `cmd/`, `internal/`, `interop/` | `bin/`, `*.exe` |
| `spec/`, `www/` | `*.test`, `*.out`, `coverage.*` |
| `go.mod`, `.gitignore`, `.gitattributes` | `.idea/`, `.vscode/` |
| `README.md`, `SPEC-NOTES.md` | `*.log`, `.env` |

Build artifacts are listed in `.gitignore`. After cloning, run `go build -o
bin/ ./cmd/...` locally.
