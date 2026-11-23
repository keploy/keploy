# Reads the keploy/integrations ref a keploy PR's body names: the Windows twin
# of integration-ref.sh, dot-sourced by setup-private-parsers' Windows step.
#
# Get-IntegrationRefOfBody BODY
#   Returns the first word after the first `Integration-Ref:` line of BODY, or
#   $null when there is none. The tag is case-insensitive, may have spaces
#   before its colon, and may follow leading spaces and a list or quote marker
#   ("- ", "* ", "> "). Only the first word counts: "main other" is main.
#   A body edited in GitHub's web form has CRLF line ends, so every CR is read
#   as a line end, as integration-ref.sh reads it.
function Get-IntegrationRefOfBody([string]$Body) {
  if (-not $Body) { return $null }
  $line = (($Body -replace "`r", "`n") -split "`n") |
    Where-Object { $_ -imatch '^\s*([-*>]\s+)?integration-ref\s*:' } |
    Select-Object -First 1
  if (-not $line) { return $null }
  $tail = $line -replace '^[^:]+:\s*', ''
  $tokens = $tail -split '\s+', 2
  if ($tokens.Length -gt 0 -and $tokens[0]) { return $tokens[0] }
  return $null
}
