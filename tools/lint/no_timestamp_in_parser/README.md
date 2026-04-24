# notimestampinparser

A custom `go/analysis` analyzer that enforces invariant **I5** from `PLAN.md`:

> Parser record-path code must derive `ReqTimestampMock` and `ResTimestampMock`
> from `fakeconn.Chunk.ReadAt` / `WrittenAt`, not from `time.Now()`.

Calling `time.Now()` during record captures scheduler / decoder latency rather
than the actual wire event time, producing mocks whose timestamps drift from
the true request/response ordering. This analyzer is a structural guard that
fails the build any time a new `time.Now` / `time.Since` / `time.Until`
reference appears inside a parser record file.

## Scope

The analyzer inspects, and only inspects, V2 record-path files — files
that adopted the chunk-timestamp contract. Two matchers:

- `*_v2.go`               (any file whose basename ends with `_v2.go`,
  e.g. `record_v2.go`, `encode_v2.go`, `query_v2.go`)
- `**/recorder_v2/*.go`   (any Go file under a `recorder_v2/` subpackage,
  reserved for parsers that split their V2 logic out)

and excludes `*_test.go` from the scan.

The older, legacy `encode.go` / `record.go` files (e.g.
`pkg/agent/proxy/integrations/generic/encode.go`,
`pkg/agent/proxy/integrations/http/encode.go`) use `time.Now()`
extensively and are **deliberately out of scope**. Their behaviour is
the pre-V2 anti-pattern PLAN.md documents as what the V2 architecture
replaces; retrofitting them would produce a flood of false positives.

## Allowlist

Within scope, two opt-outs exist:

1. **File-level:** files named `record_legacy*.go` are exempt entirely.
   Use only for files whose name signals the legacy origin explicitly.
2. **Line- or block-level:** any single call site can be suppressed
   by placing the exact marker `// allow:time.Now` (or a block
   comment `/* allow:time.Now … */`) on the line immediately above
   the call. Intended only for log and telemetry sites where
   wall-clock time is the point, e.g.:

   ```go
   // allow:time.Now
   log.Info("handler boot", zap.Time("at", time.Now()))
   ```

Tests (`*_test.go`) are out of scope by construction — the
`_test.go` suffix exits the scope check before the rule applies.

## Running locally

```sh
# Standalone driver (fastest feedback loop):
go run ./tools/lint/no_timestamp_in_parser/cmd/no_timestamp_in_parser ./...

# Unit tests for the analyzer itself:
go test -race -count=1 ./tools/lint/no_timestamp_in_parser/...
```

The standalone driver exits non-zero on any diagnostic, suitable for
pre-commit hooks. Wiring into `golangci-lint` and CI is intentionally
deferred; this PR ships the analyzer only.
