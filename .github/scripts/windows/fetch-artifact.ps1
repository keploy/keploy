# fetch-artifact.ps1: one of this run's artifacts, downloaded as -Parts byte
# ranges at once from the signed blob URL the REST API redirects to, checked
# against the artifact's size and SHA-256, and unpacked into -Destination.
#
# GitHub keeps artifacts in Azure blob storage in the US, about 210 ms from the
# self-hosted runners, and on that path a single connection stays at 2-7
# Mbit/s whatever the machine (the Windows VM 1.8, a Linux VM on the same
# Proxmox host 2.6, the host itself 3.4, a Mac 3.4: loss on the ISP's path,
# with receive-window auto-tuning already normal), where Cloudflare downloads
# reach 334 Mbit/s. actions/download-artifact uses one connection. Eight
# ranges at once reached 30.7 Mbit/s together.
#
#   fetch-artifact.ps1 -Name docker-image-linux-amd64 -Destination <dir> [-Parts 8]
#
# Needs GITHUB_TOKEN (actions: read), GITHUB_REPOSITORY, GITHUB_RUN_ID and
# GITHUB_API_URL, as a step has them, and curl. The API is called from this
# process (HttpClient), so the token never leaves it; curl gets each signed
# URL in a config file of its own (-K), not on a command line another job on
# the machine could read, removed once it exits.
#
# Each range runs on its own: when one ends short it starts again at once, from
# its last byte, while the others go on. The signed URL is good for about 10
# minutes, and a slow range can outlive it: a range the store refuses (401,
# 403) gets a fresh one from the API and goes on from its last byte; the whole
# file is never started again for it. No clock decides: a transfer that moves
# less than -StallBytesPerSecond for -StallSeconds is stalled (curl's
# --speed-limit and --speed-time) and goes on from its last byte too; a range
# fails the download only after -MaxAttemptsWithoutProgress attempts in a row
# that got none of its bytes (each after a pause, 1 s doubling to 30 s). An
# attempt that got some is progress, however slow.
#
# The parts are joined only when each holds exactly its range, and the whole
# must have the artifact's size and the SHA-256 of the API's digest before
# anything uses it. A mismatch downloads it once more, from a fresh URL; a
# second fails loudly, naming the artifact. Then the zip is unpacked into a
# directory of its own and its files moved into -Destination.
#
# Exit 0: -Destination holds the artifact's files. 1: it does not, and says
# why: the caller downloads the artifact the usual way. 3: the artifact's bytes
# twice did not match its digest: the caller must fail, not fall back. 2:
# called wrongly.
[CmdletBinding()]
param(
    [string]$Name,
    [string]$Destination,
    [int]$Parts = 8,
    [string]$Repository = $env:GITHUB_REPOSITORY,
    [string]$RunId = $env:GITHUB_RUN_ID,
    [string]$ApiUrl = $env:GITHUB_API_URL,
    [string]$Curl,
    [int]$StallBytesPerSecond = 16384,
    [int]$StallSeconds = 60,
    [int]$MaxAttemptsWithoutProgress = 5
)
$ErrorActionPreference = 'Stop'

function Fail([int]$code, [string]$why) {
    if ($code -eq 3) { Write-Host "::error::fetch-artifact: $why" } else { Write-Host "::warning::fetch-artifact: $why" }
    exit $code
}

if (-not $Name -or -not $Destination) { Fail 2 'wants -Name and -Destination' }
if ($Parts -lt 1 -or $StallBytesPerSecond -lt 1 -or $StallSeconds -lt 1 -or $MaxAttemptsWithoutProgress -lt 1) { Fail 2 'wants -Parts, -StallBytesPerSecond, -StallSeconds and -MaxAttemptsWithoutProgress of 1 or more' }
if (-not $Repository -or -not $RunId) { Fail 2 'wants GITHUB_REPOSITORY and GITHUB_RUN_ID (or -Repository and -RunId)' }
if (-not $env:GITHUB_TOKEN) { Fail 2 'wants GITHUB_TOKEN' }
if (-not $ApiUrl) { $ApiUrl = 'https://api.github.com' }
$ApiUrl = $ApiUrl.TrimEnd('/')
if (-not $Curl) { $Curl = if ($env:OS -eq 'Windows_NT') { 'curl.exe' } else { 'curl' } }

# Q: a value for a curl config file, double-quoted, its backslashes and quotes escaped.
function Q([string]$s) { '"' + ($s -replace '\\', '\\' -replace '"', '\"') + '"' }

# Start-Curl CONFIG ARGS: curl, reading CONFIG (lines of a curl config file)
# from a file of its own (-K; written without a BOM, which curl would read as
# part of the first option's name), with ARGS. Process.Start holds the
# process's handle from its creation, so its exit code can be read after it
# exits, under Windows PowerShell 5.1 too.
function Start-Curl([string[]]$config, [string[]]$curlArgs) {
    $cfg = Join-Path $work ("curl-" + [guid]::NewGuid().ToString('N') + ".cfg")
    [IO.File]::WriteAllText($cfg, ($config -join "`n") + "`n", (New-Object Text.UTF8Encoding $false))
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = $Curl
    $psi.Arguments = (@('-K', $cfg) + $curlArgs | ForEach-Object { '"' + $_ + '"' }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [Diagnostics.Process]::Start($psi)
    $script:procs += $p
    [pscustomobject]@{ Proc = $p; Cfg = $cfg; Out = $p.StandardOutput.ReadToEndAsync(); Err = $p.StandardError.ReadToEndAsync() }
}
function Wait-Curl($c) {
    $c.Proc.WaitForExit()
    Remove-Item -LiteralPath $c.Cfg -Force -ErrorAction SilentlyContinue
    [pscustomobject]@{ Code = $c.Proc.ExitCode; Out = "$($c.Out.Result)".Trim(); Err = "$($c.Err.Result)".Trim() }
}

# Invoke-Api URL: a GET of the API, not following a redirect; its status,
# body and Location (as sent: a signed URL's query is not re-encoded), or
# status 0 and why when there was no answer.
$script:procs = @()
Add-Type -AssemblyName System.Net.Http
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$handler = New-Object Net.Http.HttpClientHandler
$handler.AllowAutoRedirect = $false
$http = New-Object Net.Http.HttpClient($handler)
$http.DefaultRequestHeaders.Authorization = New-Object Net.Http.Headers.AuthenticationHeaderValue('Bearer', $env:GITHUB_TOKEN)
$http.DefaultRequestHeaders.Accept.ParseAdd('application/vnd.github+json')
$http.DefaultRequestHeaders.UserAgent.ParseAdd('keploy-ci-fetch-artifact')
$http.DefaultRequestHeaders.Add('X-GitHub-Api-Version', '2022-11-28')
function Invoke-Api([string]$url) {
    try {
        $resp = $http.GetAsync($url).GetAwaiter().GetResult()
        $body = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        $loc = $null
        if ($resp.Headers.Location) { $loc = $resp.Headers.Location.OriginalString }
        [pscustomobject]@{ Code = [int]$resp.StatusCode; Body = $body; Location = $loc; Err = '' }
    } catch {
        $e = $_.Exception
        while ($e.InnerException) { $e = $e.InnerException }
        [pscustomobject]@{ Code = 0; Body = ''; Location = $null; Err = $e.Message }
    }
}

# The artifact: the newest of this run's by that name.
$r = Invoke-Api "$ApiUrl/repos/$Repository/actions/runs/$RunId/artifacts?name=$([uri]::EscapeDataString($Name))&per_page=100"
if ($r.Code -ne 200) { Fail 1 "listing run $RunId's artifacts: HTTP $($r.Code) $($r.Err) $($r.Body)" }
$art = @((ConvertFrom-Json $r.Body).artifacts | Where-Object { $_.name -eq $Name -and -not $_.expired } | Sort-Object { [long]$_.id } -Descending)[0]
if (-not $art) { Fail 1 "no artifact named $Name in run $RunId" }
[long]$size = $art.size_in_bytes
if ($size -le 0) { Fail 1 "artifact $Name ($($art.id)) has size $size" }
$want = $null
if ($art.digest -and "$($art.digest)" -match '^sha256:([0-9a-f]{64})$') { $want = $Matches[1] }
$what = "artifact $Name ($($art.id), $size bytes)"

$work = Join-Path $(if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [IO.Path]::GetTempPath() }) ("fetch-artifact-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null

# Get-SignedUrl: where the API redirects the artifact's zip, or $null.
function Get-SignedUrl {
    $r = Invoke-Api "$ApiUrl/repos/$Repository/actions/artifacts/$($art.id)/zip"
    if ($r.Code -ne 302 -or -not $r.Location) {
        Write-Host "fetch-artifact: no signed URL for $what from the API: HTTP $($r.Code) $($r.Err)"
        return $null
    }
    $r.Location
}

function Get-Length([string]$path) { (Get-Item -LiteralPath $path).Length }

# Get-Zip PASS: the artifact's zip, every range whole; its path. Exits 1 when a
# range cannot be had.
function Get-Zip([int]$pass) {
    $began = Get-Date
    $n = [int][Math]::Min([long]$Parts, $size)
    $chunk = [long][Math]::Ceiling($size / $n)
    $script:url = Get-SignedUrl
    $ranges = for ($i = 0; $i -lt $n; $i++) {
        $from = $i * $chunk
        $to = [Math]::Min($size, $from + $chunk) - 1
        $file = Join-Path $work "pass$pass.part$i"
        [IO.File]::WriteAllBytes($file, [byte[]]@())
        [pscustomobject]@{ I = $i; From = $from; To = $to; Len = $to - $from + 1; File = $file; More = "$file.more"; Dry = 0; Have = 0; C = $null; Url = $null; NotBefore = $began; Attempts = 0 }
    }
    Write-Host "fetch-artifact: $what in $n ranges"
    while ($true) {
        $left = 0
        foreach ($x in $ranges) {
            if ($x.C) {
                if (-not $x.C.Proc.HasExited) { $left++; continue }
                $r = Wait-Curl $x.C
                $x.C = $null
                $code = $r.Out
                $why = "curl exit $($r.Code), HTTP $code $($r.Err)".Trim()
                if ($code -eq '200') { Fail 1 "the blob store answered a byte range of $what with the whole file (HTTP 200): it does not serve ranges" }
                if ($code -eq '206' -and (Test-Path -LiteralPath $x.More)) {
                    # What came, even when the transfer broke off: appended,
                    # never past the range's end.
                    $got = [IO.File]::ReadAllBytes($x.More)
                    if ($got.Length -gt ($x.Len - $x.Have)) { Fail 1 "range $($x.I) of $what came back longer than asked ($($got.Length) bytes for $($x.Len - $x.Have))" }
                    $f = [IO.File]::Open($x.File, 'Append', 'Write')
                    try { $f.Write($got, 0, $got.Length) } finally { $f.Dispose() }
                }
                Remove-Item -LiteralPath $x.More -Force -ErrorAction SilentlyContinue
                if ($code -eq '401' -or $code -eq '403') {
                    # The signed URL ran out (or was refused): a fresh one,
                    # unless another range has fetched it since this one began.
                    if ($x.Url -eq $script:url) { $script:url = Get-SignedUrl }
                }
                $now = Get-Length $x.File
                if ($now -gt $x.Have) {
                    $x.Dry = 0
                    if ($now -lt $x.Len) { Write-Host "fetch-artifact: range $($x.I) broke off at $now of $($x.Len) bytes ($why); going on from there" }
                } elseif ($now -lt $x.Len) {
                    $x.Dry++
                    Write-Host "fetch-artifact: range $($x.I) got nothing ($why), $($x.Dry) attempt(s) in a row"
                    if ($x.Dry -ge $MaxAttemptsWithoutProgress) { Fail 1 "range $($x.I) of $what got nothing in $($x.Dry) attempts in a row ($why)" }
                    $x.NotBefore = (Get-Date).AddSeconds([Math]::Min(30, [Math]::Pow(2, $x.Dry - 1)))
                }
            }
            $x.Have = Get-Length $x.File
            if ($x.Have -ge $x.Len) { continue }
            $left++
            if ((Get-Date) -lt $x.NotBefore) { continue }
            if (-not $script:url) { $script:url = Get-SignedUrl }
            if (-not $script:url) {
                $x.Dry++
                if ($x.Dry -ge $MaxAttemptsWithoutProgress) { Fail 1 "no signed URL for $what in $($x.Dry) attempts in a row" }
                $x.NotBefore = (Get-Date).AddSeconds([Math]::Min(30, [Math]::Pow(2, $x.Dry - 1)))
                continue
            }
            $x.Url = $script:url
            $x.Attempts++
            $cfg = @("url = $(Q $x.Url)", "range = $(Q "$($x.From + $x.Have)-$($x.To)")", "output = $(Q $x.More)")
            $x.C = Start-Curl $cfg @('-sS', '--connect-timeout', '30', '--speed-limit', "$StallBytesPerSecond", '--speed-time', "$StallSeconds", '-w', '%{http_code}')
        }
        if ($left -eq 0) { break }
        Start-Sleep -Milliseconds 100
    }
    $zip = Join-Path $work "pass$pass.zip"
    $out = [IO.File]::Open($zip, 'CreateNew', 'Write')
    try {
        foreach ($x in $ranges) {
            $in = [IO.File]::OpenRead($x.File)
            try { $in.CopyTo($out) } finally { $in.Dispose() }
            Remove-Item -LiteralPath $x.File -Force
        }
    } finally { $out.Dispose() }
    $secs = ((Get-Date) - $began).TotalSeconds
    $attempts = ($ranges | Measure-Object -Property Attempts -Sum).Sum
    Write-Host ("fetch-artifact: {0} bytes of {1} in {2:N1} s ({3:N1} Mbit/s), {4} range requests for {5} ranges" -f $size, $Name, $secs, ($size * 8 / 1e6 / [Math]::Max($secs, 0.001)), $attempts, $n)
    $zip
}

try {
    $zip = $null
    foreach ($pass in 1, 2) {
        $z = Get-Zip $pass
        $got = Get-Length $z
        if ($got -ne $size) { Fail 1 "joined $got bytes of $what, which has $size" }
        if (-not $want) {
            Write-Host "fetch-artifact: the API gave no digest for ${Name}: checked by its size only"
            $zip = $z
            break
        }
        $sum = (Get-FileHash -LiteralPath $z -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($sum -eq $want) {
            Write-Host "fetch-artifact: sha256 $sum, the artifact's digest"
            $zip = $z
            break
        }
        Remove-Item -LiteralPath $z -Force
        if ($pass -eq 2) { Fail 3 "$what downloaded twice with a SHA-256 that is not its digest (last $sum, digest $want)" }
        Write-Host "fetch-artifact: $what came down with SHA-256 $sum, not its digest ${want}: downloading it once more"
    }

    $unpacked = Join-Path $work 'unpacked'
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [IO.Compression.ZipFile]::ExtractToDirectory($zip, $unpacked)
    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    Get-ChildItem -LiteralPath $unpacked -Force | ForEach-Object {
        $target = Join-Path $Destination $_.Name
        if (Test-Path -LiteralPath $target) { Remove-Item -LiteralPath $target -Recurse -Force }
        Move-Item -LiteralPath $_.FullName -Destination $target
    }
    Write-Host "fetch-artifact: $Name unpacked into $Destination"
    exit 0
} catch {
    Fail 1 "$what`: $($_.Exception.Message)"
} finally {
    # A failure leaves no curl of this behind, writing into what is removed.
    foreach ($p in $script:procs) {
        if (-not $p.HasExited) {
            try { $p.Kill() } catch { }
            $p.WaitForExit()
        }
    }
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
