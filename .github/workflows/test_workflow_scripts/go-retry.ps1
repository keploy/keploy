# PowerShell companion to go-retry.sh, for the windows lanes.
#
# Exists because setup-private-parsers has a pwsh arm that reaches github.com
# over SSH exactly as the bash arm does (GOPRIVATE + an insteadOf rewrite), and
# github.com sheds SSH sessions under load with a rejection that reads as
# permanent. Wrapping only the bash arm would fix half the problem.
#
# A FILE rather than a copy inlined into each pwsh step that needs it: the
# transient set has to stay in step with go-retry.sh's GO_RETRY_RE, and there
# are two pwsh steps here that run go. Keeping one copy per language means a
# future edit cannot update one and miss another, silently leaving a windows
# lane un-retried.
#
# Read go-retry.sh first: the reasoning for WHICH failures are transient, and
# why retrying anything else is harmful, lives there and is not repeated here.

# Mirrors GO_RETRY_RE in go-retry.sh, including the proxy.golang.org HTTP/2
# patterns that file was originally written for — `go get` on a private module
# still resolves its transitive dependencies through the proxy, so a windows
# lane would otherwise hard-fail on the exact class every linux lane retries.
$GoRetryTransient = 'stream error: stream id|INTERNAL_ERROR; received from peer|tls handshake timeout|i/o timeout|connection reset by peer|500 Internal Server Error|502 Bad Gateway|503 Service Unavailable|504 Gateway Time-?out|429 Too Many Requests|too many requests|read "https?://[^"]*": unexpected EOF|permission denied \(publickey\)|kex_exchange_identification|connection closed by remote host'

# Invoke-GoWithRetry runs `go @GoArgs`, retrying only transient failures.
# Returns $true on success and $false otherwise; callers decide whether a
# failure is fatal.
#
# One exception to that contract: if `go` is not on PATH at all this THROWS
# (CommandNotFoundException under the Actions default
# $ErrorActionPreference='stop') rather than returning $false. That is the right
# outcome and is left alone — setup-go runs earlier in this same action, so a
# missing go means the action itself is broken, and no number of retries will
# find it.
#
# Deliberately never switches GOPROXY to direct, which is where this differs
# from go-retry.sh's default schedule. Every command wrapped here resolves a
# whole module graph, and flipping all of it to direct makes a renamed,
# deleted or retagged upstream fail PERMANENTLY where the proxy would still
# serve it — the same reason check-deprecated-deps.sh pins
# GO_RETRY_DIRECT_FROM=99 for `go list -m -u all`.
function Invoke-GoWithRetry {
  param(
    [string[]]$GoArgs,
    [int]$MaxAttempts = 4,
    [int]$BackoffSeconds = 5,
    [string]$What = "go command",
    # Suppresses go's own output, for speculative calls whose failure is the
    # ordinary case. Retry notices are still printed: a silent stall is what
    # makes a shed SSH session hard to recognise afterwards.
    [switch]$Quiet
  )

  # ::error:: turns into a job annotation, so it is reserved for failures the
  # caller treats as fatal. A speculative call passes -Quiet precisely because
  # its failure is the ordinary case — a PR with no matching branch in the
  # other repo — and annotating that would stamp a red error onto a green job,
  # which is the "reads as a code failure in an unrelated PR" outcome
  # go-retry.sh exists to prevent.
  $annotate = if ($Quiet) { "" } else { "::error::" }

  if ($MaxAttempts -lt 1) {
    # Mirrors go-retry.sh: never report success for a command that was never
    # run, however the caller mis-set the bound.
    Write-Host "::error::$What was never attempted (MaxAttempts=$MaxAttempts)"
    return $false
  }

  $attempt = 1
  $backoff = $BackoffSeconds
  while ($true) {
    # Reset explicitly. Tee-Object -Variable does NOT clear the variable when
    # the pipeline produces nothing, so an attempt that fails silently would
    # otherwise inherit the PREVIOUS attempt's text and be classified by it.
    $captured = $null

    # Streamed, not buffered. Assigning the pipeline to a variable first
    # (`$out = & go … | Tee-Object …`) forces it to run to completion before
    # anything is echoed, so a step killed at timeout-minutes would replay
    # nothing — losing the output of the very hang this file exists to
    # diagnose. Piping straight into ForEach-Object prints each line as it
    # arrives while Tee-Object still captures it for the match below.
    #
    # 2>&1 merges go's stderr into the success stream, which is safe here for a
    # reason specific to pwsh: 6+ surfaces native stderr as plain strings,
    # whereas Windows PowerShell 5.1 wraps each line in a NativeCommandError
    # that $ErrorActionPreference='stop' would make terminating. The shell is
    # pwsh, so the strings are what arrive.
    #
    # Separately, and for a different reason, a non-zero exit does not
    # terminate the step either: $PSNativeCommandUseErrorActionPreference is
    # $false on pwsh 7.4, so the exit code only lands in $LASTEXITCODE. Both
    # facts have to hold for the retry decision below to be reached at all.
    & go @GoArgs 2>&1 | Tee-Object -Variable captured | ForEach-Object {
      if (-not $Quiet) { Write-Host $_ }
    }
    $code = $LASTEXITCODE
    if ($code -eq 0) { return $true }

    $text = ($captured | Out-String)
    if ($text -notmatch $GoRetryTransient) {
      Write-Host "$annotate$What failed (exit $code) and the output does not look transient - not retrying"
      return $false
    }
    if ($attempt -ge $MaxAttempts) {
      Write-Host "$annotate$What failed after $MaxAttempts attempts"
      return $false
    }
    Write-Host "$What attempt $attempt failed transiently; retrying in $backoff s (attempt $($attempt + 1)/$MaxAttempts)"
    Start-Sleep -Seconds $backoff
    $backoff = $backoff * 2
    $attempt = $attempt + 1
  }
}
