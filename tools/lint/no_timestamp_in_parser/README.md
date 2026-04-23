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

The analyzer inspects, and only inspects, files matching either:

- `**/recorder/*.go`   (any Go file under a `recorder/` directory)
- `encode*.go`         (any file whose basename begins with `encode`)

and excludes `*_test.go` from the scan.

In practice this maps to:

- `pkg/agent/proxy/integrations/**/recorder/*.go`
- `pkg/agent/proxy/integrations/**/encode*.go`

## Allowlist

Within scope, two opt-outs exist:

1. **File-level:** files named `record_legacy*.go` are exempt entirely. The
   legacy record path predates invariant I5 and will be retired, not fixed.
2. **Line-level:** any single call site can be suppressed by placing the
   exact marker `// allow:time.Now` on the line immediately above the call.
   Intended only for log and telemetry sites where wall-clock time is the
   point, e.g.:

   ```go
   // allow:time.Now
   log.Info("handler boot", zap.Time("at", time.Now()))
   ```

Tests (`*_test.go`) under `recorder/` are out of scope by construction — the
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
