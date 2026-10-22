# machine-cache.ps1: a file a job on this machine built, kept for the other
# jobs on it, so that they do not download it again.
#
# The four self-hosted Windows runners share ONE machine. build-windows-amd64
# builds keploy.exe (140 MB; its artifact zip is 45 MB) there and uploads it as
# the build-windows artifact, and the Windows docker lane, on the same machine,
# downloaded it back: twice per job (its record and its replay binary are the
# same artifact), at 0.1-0.65 MB/s, 73-480 s each (runs 39021950515 to
# 39052648438), where the same machine uploads it in about 10 s. With the
# image's download as well (170-613 s), that left the lane's test a few minutes
# of its 30-minute cap, and twice none.
#
# An entry is keyed by its content: a directory per NAME and the file's SHA-256
# under -Root, by default <drive>\keploy-ci-cache, beside the runners' own
# directories (D:\actions-runner-N), on the same drive, so a put is a rename,
# and outside any runner's workspace, which its checkout cleans. The commit
# does not name the build: the integrations ref it builds with moves, and a
# re-run of the same commit builds again. The builder gives its file's SHA-256
# to the jobs that take it, and a get copies a file only when what it copied
# hashes to that: a torn, stale or tampered entry is a miss, not a binary. A
# put writes into a directory of its own and renames it into place, and puts
# again over an entry that does not hold its file whole.
#
#   machine-cache.ps1 -Put -Name build-windows -File keploy.exe
#   machine-cache.ps1 -Get -Name build-windows -Key <sha256> -File keploy.exe -Destination bin\keploy.exe
#   machine-cache.ps1 -Prune -MaxEntries 16 -KeepHours 3 -MaxAgeDays 1
#
# A put prints sha256=<hex> (for $GITHUB_OUTPUT) and exits 0 when the entry is
# there afterwards (made now, or by another job first), 1 otherwise. A get
# prints hit=true or hit=false and exits 0 either way, whatever became of the
# entry (never put, evicted, removed or replaced while it was read, not whole):
# a miss is the caller's to fill, from the artifact (.github/actions/
# windows-binary). Only a call that is wrong (a bad -Name or -Key) exits 1.
#
# A prune removes, per name, an entry older than -MaxAgeDays, and one past the
# -MaxEntries newest that is older than -KeepHours; and leftovers of puts that
# died, and directories that are not entries (an older layout's), once they
# are older than -MaxAgeDays. This is a performance policy, not a guarantee:
# an entry a run still needs can be evicted under load, and costs that run a
# download, never its job. 16 entries and 3 hours cover the gap from a build
# to its lanes' get, which was 4.8 min median, 76.5 min at most, over 101 runs
# of 2026-10-14 and 2026-10-16. An entry younger than -KeepHours is kept
# whatever the count, so a name holds the 16 newest plus every build of the
# last 3 hours: up to 37 same-repo runs started in one 3-hour window over
# 2026-10-14 to 10-17, so about 53 entries, 7.5 GB at 141 MB each, at the
# busiest. Nothing may depend on an entry being
# there: if something ever does, eviction needs a lease (an in-use marker that
# prune skips), not a longer age.
[CmdletBinding()]
param(
    [switch]$Put,
    [switch]$Get,
    [switch]$Prune,
    [string]$Name,
    [string]$Key,
    [string]$File,
    [string]$Destination,
    [string]$Root,
    [int]$MaxEntries = 16,
    [int]$KeepHours = 3,
    [int]$MaxAgeDays = 1
)
$ErrorActionPreference = 'Stop'

if (-not $Root) {
    # <drive>\keploy-ci-cache: the drive the runner's workspace is on.
    $ws = $env:GITHUB_WORKSPACE
    if (-not $ws) { throw 'machine-cache.ps1: no -Root and no GITHUB_WORKSPACE to put it beside' }
    $Root = Join-Path ([IO.Path]::GetPathRoot($ws)) 'keploy-ci-cache'
}

function Get-Sha256([string]$path) {
    (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
}

# Test-Name: a name is one path segment of letters, digits, '_' and '-'.
function Test-Name([string]$n) { $n -match '^[A-Za-z0-9_-]+$' }
# Test-Key: a key is a SHA-256, in lowercase hex.
function Test-Key([string]$k) { $k -cmatch '^[0-9a-f]{64}$' }

if ($Prune) {
    if (-not (Test-Path -LiteralPath $Root)) { exit 0 }
    $now = Get-Date
    $cutoff = $now.AddDays(-$MaxAgeDays)
    $young = $now.AddHours(-$KeepHours)
    Get-ChildItem -LiteralPath $Root -Directory -Force -ErrorAction SilentlyContinue | ForEach-Object {
        $entries = @(Get-ChildItem -LiteralPath $_.FullName -Directory -Force -ErrorAction SilentlyContinue)
        # Not an entry: a put's own directory, left by one that died, or an
        # older layout's (keyed by commit), once it is -MaxAgeDays old.
        $entries | Where-Object { -not (Test-Key $_.Name) -and $_.LastWriteTime -lt $cutoff } | ForEach-Object {
            Write-Host "machine-cache: removing $($_.FullName), which is not an entry, older than $MaxAgeDays day(s)"
            Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
        }
        $newer = 0
        $entries | Where-Object { Test-Key $_.Name } | Sort-Object LastWriteTime -Descending | ForEach-Object {
            $why = $null
            if ($_.LastWriteTime -lt $cutoff) { $why = "older than $MaxAgeDays day(s)" }
            elseif ($newer -ge $MaxEntries -and $_.LastWriteTime -lt $young) { $why = "past the newest $MaxEntries, and older than $KeepHours hour(s)" }
            $newer++
            if ($why) {
                Write-Host "machine-cache: removing $($_.FullName): $why"
                Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    }
    exit 0
}

if (-not $File) { throw 'machine-cache.ps1: -File is required' }
if (-not (Test-Name $Name)) { throw "machine-cache.ps1: -Name is one path segment of letters, digits, '_' and '-', got '$Name'" }
$leaf = Split-Path -Leaf $File
$dir = Join-Path $Root $Name

if ($Get) {
    if (-not (Test-Key $Key)) { throw "machine-cache.ps1: -Key is the file's SHA-256 in lowercase hex, got '$Key'" }
    if (-not $Destination) { throw 'machine-cache.ps1: -Get needs -Destination' }
    $cached = Join-Path (Join-Path $dir $Key) $leaf
    $hit = $false
    if (Test-Path -LiteralPath $cached -PathType Leaf) {
        $destDir = Split-Path -Parent $Destination
        if ($destDir) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }
        try {
            Copy-Item -LiteralPath $cached -Destination $Destination -Force
            # The copy is what is checked: an entry a prune or a put
            # replaced while it was read is caught here.
            $got = Get-Sha256 $Destination
            if ($got -eq $Key) {
                $hit = $true
                Write-Host "machine-cache: $Name/$leaf ($Key) from this machine"
            } else {
                Write-Host "::warning::machine-cache: $cached does not hash to its key ($got); fetching it instead"
            }
        } catch {
            # Gone since the check (a prune, a replacing put), or held by
            # one: a miss like any other.
            Write-Host "::warning::machine-cache: $cached could not be read ($($_.Exception.Message)); fetching it instead"
        }
        if (-not $hit) { Remove-Item -LiteralPath $Destination -Force -ErrorAction SilentlyContinue }
    } else {
        Write-Host "machine-cache: no $Name/$leaf ($Key) on this machine"
    }
    "hit=$($hit.ToString().ToLowerInvariant())"
    exit 0
}

if ($Put) {
    if (-not (Test-Path -LiteralPath $File -PathType Leaf)) { throw "machine-cache.ps1: $File not found" }
    $sum = Get-Sha256 $File
    $entry = Join-Path $dir $sum
    $have = Join-Path $entry $leaf
    if (Test-Path -LiteralPath $have -PathType Leaf) {
        $there = $null
        try { $there = Get-Sha256 $have } catch {
            # Held open by another process (a get copying it, say): left as
            # it is. A get checks what it copies, so one that is not whole is
            # a miss there, not a binary.
            Write-Host "machine-cache: $Name/$leaf ($sum) is there, held by another process ($($_.Exception.Message)); leaving it"
            "sha256=$sum"
            exit 0
        }
        if ($there -eq $sum) {
            Write-Host "machine-cache: $Name/$leaf ($sum) is already on this machine"
            "sha256=$sum"
            exit 0
        }
    }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    $tmp = Join-Path $dir (".$sum.tmp-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Copy-Item -LiteralPath $File -Destination (Join-Path $tmp $leaf) -Force
        if ((Get-Sha256 (Join-Path $tmp $leaf)) -ne $sum) { throw "machine-cache.ps1: the copy of $File into the cache does not hash to $sum" }
        if (Test-Path -LiteralPath $entry) {
            # An entry that does not hold its file whole (torn, or
            # corrupt): this put's replaces it. A whole one another job put
            # meanwhile is left as it is.
            if ((Test-Path -LiteralPath $have -PathType Leaf) -and (Get-Sha256 $have) -eq $sum) {
                Write-Host "machine-cache: $Name/$leaf ($sum) was put by another job meanwhile"
                "sha256=$sum"
                exit 0
            }
            Write-Host "machine-cache: replacing $entry, which does not hold its file whole"
            Remove-Item -LiteralPath $entry -Recurse -Force
        }
        try {
            Rename-Item -LiteralPath $tmp -NewName $sum
            Write-Host "machine-cache: kept $Name/$leaf ($sum) on this machine"
        } catch {
            # Another job put it first: theirs stands, if whole.
            if (-not ((Test-Path -LiteralPath $have -PathType Leaf) -and (Get-Sha256 $have) -eq $sum)) { throw }
            Write-Host "machine-cache: $Name/$leaf ($sum) was put by another job meanwhile"
        }
    } finally {
        if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue }
    }
    "sha256=$sum"
    exit 0
}

throw 'machine-cache.ps1: give -Get, -Put or -Prune'
