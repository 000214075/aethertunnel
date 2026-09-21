<#
.SYNOPSIS
    End-to-end smoke test for AetherTunnel.

.DESCRIPTION
    Starts a server, an owner client and a visitor client as real processes against
    local echo services, then exercises every proxy type and the optional security
    layers:

      tcp        a byte stream through a published port
      udp        datagrams through a published port, one session per source address
      http       virtual hosting on the shared listener, selected by Host header
      https      the same over TLS
      stcp       a private tunnel reached by a visitor, authenticated with the secret
      sudp       a private tunnel carrying datagrams
      xtcp       a visitor that first tries a direct path and otherwise relays
      socks5     an exit reached with curl --socks5-hostname, and a target outside
                 its allow_targets list refused with a SOCKS5 error code
      acl        a proxy whose own allow_cidrs refuses one loopback address and
                 serves the other, with the refusal in the audit log
      ban        a second server that bans a source after repeated authentication
                 failures, refuses an accepted client from that source, and lets it
                 back in once the window passes
      dht        announcements signed by the server, verified by a reader that names
                 the key, and refused by a reader that names another one
      tls        the control port wrapped in TLS
      identity   an Ed25519 client identity the server requires
      post_quantum  X25519 with ML-KEM-768 and a per-stream key
      nizk       a visitor that proves knowledge of the proxy secret

    It also checks the health probes, the Prometheus endpoint and the audit log.

    Run it from the repository root:  powershell -File scripts\smoke-test.ps1

.PARAMETER Root
    Working directory for the generated configuration, logs and certificates.
    Defaults to a fresh directory under the system temporary directory.

.PARAMETER Keep
    Leave the working directory and any running process in place for inspection.

.PARAMETER ServerExe
    Exercise this server binary instead of building one. ClientExe and HelperExe work
    the same way; anything not given is built from this checkout. The release workflow
    passes the artifacts it has just published, so the checks run against what was
    uploaded rather than against a fresh build of the same source.
#>
[CmdletBinding()]
param(
    [string]$Root = (Join-Path $env:TEMP ("aether-smoke-" + [guid]::NewGuid().ToString('N').Substring(0, 8))),
    [switch]$Keep,
    # Appends each step and verdict here as it happens. stdout is buffered when the
    # script is run with its output redirected, so this is what shows where a run is.
    [string]$ProgressLog = '',
    # Binaries to exercise instead of building them from this checkout. The release
    # workflow hands in the artifacts it just published, which is the only way to show
    # that what was uploaded is what was tested. Anything not handed in is built.
    [string]$ServerExe = '',
    [string]$ClientExe = '',
    [string]$HelperExe = ''
)

$ErrorActionPreference = 'Stop'
$script:Failures = @()
$script:Passes = 0
$script:Processes = @()

function Write-Step([string]$Message) {
    Write-Host ""
    Write-Host "== $Message" -ForegroundColor Cyan
    Write-Progress-Line ("== " + $Message)
}

# Add-Content flushes on every call, so the progress file shows where a run is even
# when stdout is redirected and therefore buffered.
function Write-Progress-Line([string]$Line) {
    if (-not $ProgressLog) { return }
    try {
        Add-Content -Path $ProgressLog -Value ((Get-Date).ToString('HH:mm:ss') + ' ' + $Line) -Encoding UTF8 -ErrorAction Stop
    } catch {
        # A missing progress log must not fail the run.
    }
}

function Test-Check {
    param([string]$Name, [scriptblock]$Body)

    try {
        $result = & $Body
        if ($result -eq $false) {
            throw "check returned false"
        }
        Write-Host ("   PASS  " + $Name) -ForegroundColor Green
        Write-Progress-Line ("PASS  " + $Name)
        $script:Passes++
    } catch {
        Write-Host ("   FAIL  " + $Name + " -- " + $_.Exception.Message) -ForegroundColor Red
        Write-Progress-Line ("FAIL  " + $Name + " -- " + $_.Exception.Message)
        $script:Failures += $Name
    }
}

function Wait-ForPort {
    param([int]$Port, [int]$TimeoutSeconds = 15, [string]$Host_ = '127.0.0.1')

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $client = New-Object System.Net.Sockets.TcpClient
        try {
            $client.Connect($Host_, $Port)
            $client.Close()
            return $true
        } catch {
            Start-Sleep -Milliseconds 120
        } finally {
            if ($client) { $client.Dispose() }
        }
    }
    throw "port $Port did not open within $TimeoutSeconds seconds"
}

function Start-Background {
    param([string]$FilePath, [string[]]$Arguments = @(), [string]$LogPath, [string]$WorkingDirectory)

    # Start-Process rejects an empty -ArgumentList, so only pass it when there is
    # something to pass.
    $parameters = @{
        FilePath               = $FilePath
        WorkingDirectory       = $WorkingDirectory
        RedirectStandardOutput = $LogPath
        RedirectStandardError  = ($LogPath + '.err')
        PassThru               = $true
        WindowStyle            = 'Hidden'
    }
    if ($Arguments -and $Arguments.Count -gt 0) { $parameters['ArgumentList'] = $Arguments }

    $process = Start-Process @parameters
    $script:Processes += $process
    return $process
}

# The binaries log to standard error, and standard output stays empty except for the
# one-shot subcommands. Both files are read so a check sees everything, and the
# result is always a string: an empty file would otherwise return $null, and
# "if ($null -notmatch 'pattern')" is false, which would pass every assertion below
# without checking anything.
function Read-Log {
    param([string]$LogPath)

    $text = ''
    foreach ($path in @($LogPath, ($LogPath + '.err'))) {
        if (-not (Test-Path $path)) { continue }
        $content = Get-Content -Raw -Path $path -ErrorAction SilentlyContinue
        if ($content) { $text += $content }
    }
    return $text
}

function Invoke-TcpEcho {
    param([int]$Port, [string]$Payload, [int]$ExpectedLength = 0, [string]$Host_ = '127.0.0.1', [string]$LocalAddress = '')

    if ($ExpectedLength -eq 0) { $ExpectedLength = $Payload.Length }

    $client = New-Object System.Net.Sockets.TcpClient
    try {
        # A proxy with an allow list is tested from a second loopback address, so
        # the source has to be chosen before the connection is opened.
        if ($LocalAddress) {
            $client.Client.Bind((New-Object System.Net.IPEndPoint([System.Net.IPAddress]::Parse($LocalAddress), 0)))
        }
        $client.Connect($Host_, $Port)
        $client.ReceiveTimeout = 8000
        $stream = $client.GetStream()

        $bytes = [System.Text.Encoding]::ASCII.GetBytes($Payload)
        $stream.Write($bytes, 0, $bytes.Length)
        $stream.Flush()

        $buffer = New-Object byte[] 4096
        $received = New-Object System.Collections.Generic.List[byte]
        $deadline = (Get-Date).AddSeconds(8)
        while ($received.Count -lt $ExpectedLength -and (Get-Date) -lt $deadline) {
            try {
                $count = $stream.Read($buffer, 0, $buffer.Length)
            } catch {
                break
            }
            if ($count -le 0) { break }
            for ($index = 0; $index -lt $count; $index++) { $received.Add($buffer[$index]) }
        }
        return [System.Text.Encoding]::ASCII.GetString($received.ToArray())
    } finally {
        $client.Dispose()
    }
}

function Invoke-UdpEcho {
    param([int]$Port, [string]$Payload, [int]$TimeoutMs = 4000, [string]$Host_ = '127.0.0.1')

    $client = New-Object System.Net.Sockets.UdpClient
    try {
        $client.Client.ReceiveTimeout = $TimeoutMs
        $client.Connect($Host_, $Port)
        $bytes = [System.Text.Encoding]::ASCII.GetBytes($Payload)

        # The first datagrams of a new address can be dropped while the datagram
        # session is established, so retry the way a real client would.
        $deadline = (Get-Date).AddSeconds(6)
        while ((Get-Date) -lt $deadline) {
            [void]$client.Send($bytes, $bytes.Length)
            try {
                $remote = New-Object System.Net.IPEndPoint([System.Net.IPAddress]::Any, 0)
                $reply = $client.Receive([ref]$remote)
                return [System.Text.Encoding]::ASCII.GetString($reply)
            } catch {
                Start-Sleep -Milliseconds 200
            }
        }
        throw "no datagram came back"
    } finally {
        $client.Dispose()
    }
}

# Windows and Linux answer on every address in 127.0.0.0/8, so 127.0.0.2 can be used
# as a second visitor source. macOS assigns only 127.0.0.1 to lo0, and binding
# another address in the range fails, so a check that needs two source addresses asks
# first and says when it cannot run.
function Test-SecondLoopback {
    $probe = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Parse('127.0.0.2'), 0)
    try {
        $probe.Start()
        return $true
    } catch {
        return $false
    } finally {
        try { $probe.Stop() } catch { }
    }
}

# Send-StopSignal asks a process to stop the way an operator would. On Windows there
# is no SIGTERM: taskkill without /F posts a close event to the process's console,
# which the server's signal handler catches. On Linux and macOS it is kill -TERM.
# Stop-Process would be a hard kill on both and would skip the drain entirely.
function Send-StopSignal([System.Diagnostics.Process]$Process) {
    if ($env:OS -eq 'Windows_NT') {
        & taskkill /PID $Process.Id 2>&1 | Out-Null
        return
    }
    & kill -TERM $Process.Id 2>&1 | Out-Null
}

# Hold-Stream opens a visitor connection through the published port and proves the
# stream works, leaving it open for the caller to use.
function Hold-Stream([int]$Port, [string]$Payload) {
    $client = New-Object System.Net.Sockets.TcpClient('127.0.0.1', $Port)
    $client.ReceiveTimeout = 8000
    $stream = $client.GetStream()
    $bytes = [System.Text.Encoding]::ASCII.GetBytes($Payload)
    $stream.Write($bytes, 0, $bytes.Length)
    $buffer = New-Object byte[] 256
    $read = $stream.Read($buffer, 0, $buffer.Length)
    $answer = [System.Text.Encoding]::ASCII.GetString($buffer, 0, $read)
    if ($answer -ne $Payload) {
        $client.Dispose()
        throw "the stream answered '$answer' before the shutdown, want '$Payload'"
    }
    return @{ Client = $client; Stream = $stream }
}

function Invoke-Curl {
    param([string[]]$Arguments)

    $output = & curl.exe @Arguments 2>&1
    return ($output | Out-String).Trim()
}

# Read-AuditLines reads a file the server may be rotating at that moment. On Windows
# a rename onto an existing file removes the destination first, so a reader can hit a
# moment where the generation is not there at all; the read is retried instead of
# failing a check on a rotation in flight.
function Read-AuditLines {
    param([string]$Path, [int]$Attempts = 8)

    for ($attempt = 0; $attempt -lt $Attempts; $attempt++) {
        if (Test-Path $Path) {
            try { return @(Get-Content $Path -ErrorAction Stop | Where-Object { $_ -ne '' }) } catch { }
        }
        Start-Sleep -Milliseconds 200
    }
    throw "could not read $Path"
}

# Get-FileSize is the same idea for a size: a rotation renames the live file away and
# creates the next one a moment later, so the configured path is briefly not there, on
# every platform. A size that is still unreadable after the retries is a fault.
function Get-FileSize {
    param([string]$Path, [int]$Attempts = 8)

    for ($attempt = 0; $attempt -lt $Attempts; $attempt++) {
        if (Test-Path $Path) {
            try { return (Get-Item $Path -ErrorAction Stop).Length } catch { }
        }
        Start-Sleep -Milliseconds 200
    }
    throw "$Path could not be read; the server may be rotating it"
}

# Get-Metric reads one series out of /metrics. The whole body is one string, so the
# line is matched with the multiline flag; a plain match would only ever look at the
# first line. Port defaults to the main server's dashboard.
function Get-Metric {
    param([string]$Name, [int]$Port = 0)

    if ($Port -eq 0) { $Port = $dashboardPort }
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$Port/metrics")
    $match = [regex]::Match($body, '(?m)^' + [regex]::Escape($Name) + ' (-?\d+)')
    if (-not $match.Success) { throw "the metrics output has no $Name" }
    return [int]$match.Groups[1].Value
}

# Get-LabelledMetric reads one series that carries a label, for example
# aethertunnel_tunnel_http_requests_total{tunnel="web"}.
function Get-LabelledMetric {
    param([string]$Name, [string]$Label, [int]$Port = 0)

    if ($Port -eq 0) { $Port = $dashboardPort }
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$Port/metrics")
    $match = [regex]::Match($body, '(?m)^' + [regex]::Escape($Name) + '\{' + [regex]::Escape($Label) + '\} (-?\d+)')
    if (-not $match.Success) { throw "the metrics output has no $Name{$Label}" }
    return [int]$match.Groups[1].Value
}

# Get-Status reads /api/status, which is what the panel polls every two seconds. The
# token argument is the dashboard token of the server being asked, or an empty string
# for a server that does not require one.
function Get-Status([int]$Port = 0, [string]$Token = '') {
    if ($Port -eq 0) { $Port = $dashboardPort }
    $uri = "http://127.0.0.1:$Port/api/status"
    if ($Token) {
        return Invoke-RestMethod -Uri $uri -Headers @{ Authorization = "Bearer $Token" } -TimeoutSec 10
    }
    return Invoke-RestMethod -Uri $uri -TimeoutSec 10
}

# Attempt-ControlConnection opens one TCP connection to a control port from the given
# source address and closes it at once. Nothing is sent: the decisions under test are
# made before the handshake, so what the server does with the socket is the whole
# answer.
function Attempt-ControlConnection {
    param([int]$Port, [string]$From = '')

    $client = New-Object System.Net.Sockets.TcpClient
    try {
        if ($From) {
            $client.Client.Bind((New-Object System.Net.IPEndPoint([System.Net.IPAddress]::Parse($From), 0)))
        }
        $client.Connect('127.0.0.1', $Port)
        $client.Close()
        return $true
    } catch {
        return $false
    } finally {
        $client.Dispose()
    }
}

# Invoke-CurlExit reports what a request did as well as what it said, which is what
# a check that expects curl to fail needs. Like Invoke-Binary it goes through cmd,
# because a native command's standard error becomes an error record and ends the
# script under ErrorActionPreference Stop.
function Invoke-CurlExit {
    param([string[]]$Arguments)

    $line = '"curl"'
    foreach ($argument in $Arguments) { $line += ' "' + $argument + '"' }
    $raw = cmd /c ($line + ' 2>&1')
    $code = $LASTEXITCODE
    return [pscustomobject]@{ Exit = $code; Output = ($raw | Out-String).Trim() }
}

# The binaries log to standard error. PowerShell 5.1 turns a native command's
# standard error into an error record, which with ErrorActionPreference Stop ends
# the script, so one-shot invocations go through cmd and discard it.
function Invoke-Binary {
    param(
        [string]$FilePath,
        [string[]]$Arguments = @(),
        # A command that is expected to fail (a refused configuration, a failed
        # verification) reads its output from stderr instead, and does not stop the
        # run.
        [switch]$AllowFailure
    )

    if (-not $Arguments) { $Arguments = @() }
    $line = '"' + $FilePath + '"'
    foreach ($argument in $Arguments) { $line += ' "' + $argument + '"' }

    if ($AllowFailure) {
        # 2>&1 so the message a failure prints is captured too.
        $output = (cmd /c ($line + ' 2>&1') | Out-String)
        return $output
    }
    return (cmd /c ($line + ' 2>NUL') | Out-String)
}

# Windows reserves ranges of ports for Hyper-V and WinNAT, and those ports refuse
# both binds with "access permissions". A port is only usable here when both a TCP
# listener and a UDP socket can be created on it.
function Get-FreePort {
    $last = ''
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        $probe = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, 0)
        $probe.Start()
        $port = $probe.LocalEndpoint.Port
        $probe.Stop()

        try {
            $tcp = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $port)
            $tcp.Start()
            $udp = New-Object System.Net.Sockets.UdpClient($port)
            $udp.Close()
            $tcp.Stop()
            return $port
        } catch {
            $last = $_.Exception.Message
            if ($tcp) { $tcp.Stop() }
            Start-Sleep -Milliseconds 20
        }
    }
    throw "no port that accepts both tcp and udp was available (last error: $last)"
}

# --- setup --------------------------------------------------------------------

$repo = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $repo 'bin'
New-Item -ItemType Directory -Force -Path $Root, $bin | Out-Null

# TOML reads backslashes as escape sequences, so every path that goes into a
# configuration file uses forward slashes.
$RootFwd = $Root -replace '\\', '/'
$BinFwd = $bin -replace '\\', '/'

function Resolve-Go {
    $onPath = Get-Command go -ErrorAction SilentlyContinue
    if ($onPath) { return $onPath.Source }

    foreach ($candidate in @(
            'C:\Program Files\Go\bin\go.exe',
            'C:\Go\bin\go.exe',
            (Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe'))) {
        if (Test-Path $candidate) { return $candidate }
    }
    throw "the Go toolchain was not found; install it or put go.exe on PATH"
}

$goExe = ''

$serverExe = $ServerExe
$clientExe = $ClientExe
$helperExe = $HelperExe

if (-not ($serverExe -and $clientExe -and $helperExe)) {
    $goExe = Resolve-Go
    Write-Step "building the binaries"
    Push-Location $repo
    try {
        & $goExe build -o (Join-Path $bin 'aethertunnel-server.exe') . ; if ($LASTEXITCODE -ne 0) { throw "server build failed" }
        & $goExe build -o (Join-Path $bin 'aethertunnel-client.exe') ./client ; if ($LASTEXITCODE -ne 0) { throw "client build failed" }
        & $goExe build -o (Join-Path $bin 'smoketest.exe') ./scripts/smoketest ; if ($LASTEXITCODE -ne 0) { throw "helper build failed" }
    } finally {
        Pop-Location
    }
    Write-Host "   binaries in $bin"
    if (-not $serverExe) { $serverExe = Join-Path $bin 'aethertunnel-server.exe' }
    if (-not $clientExe) { $clientExe = Join-Path $bin 'aethertunnel-client.exe' }
    if (-not $helperExe) { $helperExe = Join-Path $bin 'smoketest.exe' }
}

foreach ($pair in @(@('server', $serverExe), @('client', $clientExe), @('helper', $helperExe))) {
    if (-not (Test-Path $pair[1])) { throw "the $($pair[0]) binary does not exist: $($pair[1])" }
    Write-Host ("   {0,-7} {1}" -f $pair[0], (Resolve-Path $pair[1]).Path)
}
$serverExe = (Resolve-Path $serverExe).Path
$clientExe = (Resolve-Path $clientExe).Path
$helperExe = (Resolve-Path $helperExe).Path

$controlPort = Get-FreePort
$httpPort = Get-FreePort
$httpsPort = Get-FreePort
$dashboardPort = Get-FreePort
$p2pPort = Get-FreePort
$dhtPort = Get-FreePort
$vpnPort = Get-FreePort
$tcpProxyPort = Get-FreePort
$udpProxyPort = Get-FreePort
$stcpVisitorPort = Get-FreePort
$sudpVisitorPort = Get-FreePort
$xtcpVisitorPort = Get-FreePort
$socksPort = Get-FreePort
$aclProxyPort = Get-FreePort
$poolProxyPort = Get-FreePort
$spreadProxyPort = Get-FreePort
$banControlPort = Get-FreePort
$banDashboardPort = Get-FreePort
$banProxyPort = Get-FreePort
$graceControlPort = Get-FreePort
$graceProxyPort = Get-FreePort
$graceDashboardPort = Get-FreePort

$token = 'smoke-test-token-0123456789abcdef'
$passphrase = 'smoke-test-passphrase'
$salt = 'smoke-salt'

Write-Step "starting the local services"
$services = Start-Background -FilePath $helperExe `
    -LogPath (Join-Path $Root 'services.log') -WorkingDirectory $Root
Start-Sleep -Seconds 1
$servicesJson = (Get-Content -Raw (Join-Path $Root 'services.log')).Trim()
$servicesInfo = $servicesJson | ConvertFrom-Json
$tcpEchoPort = [int]($servicesInfo.tcp -split ':')[-1]
$udpEchoPort = [int]($servicesInfo.udp -split ':')[-1]
$httpEchoPort = [int]($servicesInfo.http -split ':')[-1]
Write-Host "   tcp echo :$tcpEchoPort   udp echo :$udpEchoPort   http :$httpEchoPort"

Write-Step "generating a certificate"
Invoke-Binary -FilePath $helperExe -Arguments @('-cert-dir', $Root, '-cert-hosts', '127.0.0.1,localhost') | Out-Null
if (-not (Test-Path (Join-Path $Root 'server.crt'))) { throw "the certificate was not generated" }
$certFile = "$RootFwd/server.crt"
$keyFile = "$RootFwd/server.key"

Write-Step "writing the configuration"
$clientToml = Join-Path $Root 'client.toml'
$visitorToml = Join-Path $Root 'visitor.toml'
$serverToml = Join-Path $Root 'server.toml'
$identityFile = "$RootFwd/client-identity.key"

$commonSecurity = @"
[encryption]
enabled = true
algorithm = "xchacha20-poly1305"
passphrase = "$passphrase"
salt = "$salt"
post_quantum = true

[transport]
enable_tls = true
"@

# Every TCP connection in this run is wrapped, so the disguise is exercised by the
# control connection, every data connection and every visitor connection. The jitter
# is on for the same reason: it delays every write by a random amount, and the rest
# of the script is what proves the protocol still works with that delay in the way.
$commonObfuscation = @"
[obfuscation]
enabled = true
pad_to = 256
jitter_millis = 3
disguise = "tls-record"
"@

@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

$commonObfuscation

[identity]
enabled = true
key_file = "$identityFile"

[[proxies]]
name = "tcp-echo"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
remote_port = $tcpProxyPort

[[proxies]]
name = "udp-echo"
type = "udp"
local_ip = "127.0.0.1"
local_port = $udpEchoPort
remote_port = $udpProxyPort

[[proxies]]
name = "web"
type = "http"
local_ip = "127.0.0.1"
local_port = $httpEchoPort
domains = ["web.smoke.test"]

[[proxies]]
name = "secure-web"
type = "https"
local_ip = "127.0.0.1"
local_port = $httpEchoPort
domains = ["secure.smoke.test"]

# No domains: this one is reached as subdomain-web.smoke.test, the server's
# subdomain_host convention. The client is not told the server's setting, so it can
# only warn about it.
[[proxies]]
name = "subdomain-web"
type = "http"
local_ip = "127.0.0.1"
local_port = $httpEchoPort

[[proxies]]
name = "private"
type = "stcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
secret_key = "private-secret"
auth_method = "nizk"

[[proxies]]
name = "private-udp"
type = "sudp"
local_ip = "127.0.0.1"
local_port = $udpEchoPort
secret_key = "private-udp-secret"

[[proxies]]
name = "direct"
type = "xtcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
secret_key = "direct-secret"

# A socks5 exit: the visitor names the address, and the client dials it rather
# than its own local service. allow_targets is what bounds it.
[[proxies]]
name = "socks-exit"
type = "socks5"
remote_port = $socksPort
allow_targets = ["127.0.0.0/8"]

# A proxy whose own allow list admits only the second loopback address, so the
# refusal path is exercised from 127.0.0.1 and the accept path from 127.0.0.2.
[[proxies]]
name = "acl-echo"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
remote_port = $aclProxyPort
allow_cidrs = ["127.0.0.2/32"]
"@ | Set-Content -Path $clientToml -Encoding UTF8

@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"
# Short enough that a check can watch it happen: a visiting connection that goes
# silent for this long is dropped, while one that keeps carrying bytes is not.
idle_timeout_seconds = 2

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

$commonObfuscation

[identity]
enabled = true
key_file = "$identityFile"

[[visitors]]
name = "private"
type = "stcp"
server_name = "private"
secret_key = "private-secret"
auth_method = "nizk"
bind_addr = "127.0.0.1"
bind_port = $stcpVisitorPort

[[visitors]]
name = "private-udp"
type = "sudp"
server_name = "private-udp"
secret_key = "private-udp-secret"
bind_addr = "127.0.0.1"
bind_port = $sudpVisitorPort

[[visitors]]
name = "direct"
type = "xtcp"
server_name = "direct"
secret_key = "direct-secret"
bind_addr = "127.0.0.1"
bind_port = $xtcpVisitorPort
"@ | Set-Content -Path $visitorToml -Encoding UTF8

# A visitor that presents the wrong secret, used to prove the refusal path and
# to give the audit log something to record.
$badVisitorToml = Join-Path $Root 'bad-visitor.toml'
$badVisitorPort = Get-FreePort
@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

$commonObfuscation

[identity]
enabled = true
key_file = "$identityFile"

[[visitors]]
name = "wrong-secret"
type = "sudp"
server_name = "private-udp"
secret_key = "this-is-not-the-secret"
bind_addr = "127.0.0.1"
bind_port = $badVisitorPort
"@ | Set-Content -Path $badVisitorToml -Encoding UTF8

$identityKey = (Invoke-Binary -FilePath $clientExe -Arguments @('-config', $clientToml, '-identity')).Trim()
if (-not $identityKey) { throw "the client did not report an identity key" }
Write-Host "   client identity $identityKey"

@"
[server]
bind_addr = "127.0.0.1"
bind_port = $controlPort
auth_token = "$token"
http_port = $httpPort
https_port = $httpsPort
https_cert_file = "$certFile"
https_key_file = "$keyFile"
p2p_port = $p2pPort
# Lets a proxy that declares no domains be reached at <proxy-name>.<this value>.
subdomain_host = "smoke.test"

$commonSecurity
cert_file = "$certFile"
key_file = "$keyFile"

[identity]
enabled = true
require_identity = true
allowed_keys = ["$identityKey"]

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $dashboardPort

[metrics]
enabled = true

[audit]
enabled = true
path = "$RootFwd/audit.jsonl"

[ledger]
enabled = true
path = "$RootFwd/ledger.jsonl"
signing_key_file = "$RootFwd/ledger.key"

[dht]
enabled = true
listen_addr = "127.0.0.1:$dhtPort"
advertise_host = "127.0.0.1"
signing_key_file = "$RootFwd/dht.key"

$commonObfuscation
"@ | Set-Content -Path $serverToml -Encoding UTF8

# A client-role configuration that resolves the server address from the DHT instead
# of being told it. This is the operator path: it names a proxy, not a host.
$dhtToml = Join-Path $Root 'dht.toml'
@"
[client]
auth_token = "$token"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["127.0.0.1:$dhtPort"]
discover = "tcp-echo"
"@ | Set-Content -Path $dhtToml -Encoding UTF8

# The operator hands a reader the announcement key, so only records signed with it
# are believed.
$dhtKey = (Invoke-Binary -FilePath $serverExe -Arguments @('-config', $serverToml, '-dht-key')).Trim()
if ($dhtKey.Length -ne 64) { throw "the server did not report a usable announcement key: '$dhtKey'" }
Write-Host "   announcement key $dhtKey"

$dhtTrustedToml = Join-Path $Root 'dht-trusted.toml'
@"
[client]
auth_token = "$token"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["127.0.0.1:$dhtPort"]
discover = "tcp-echo"
require_signed = true
trusted_keys = ["$dhtKey"]
"@ | Set-Content -Path $dhtTrustedToml -Encoding UTF8

$dhtUntrustedToml = Join-Path $Root 'dht-untrusted.toml'
@"
[client]
auth_token = "$token"

[dht]
enabled = true
listen_addr = "127.0.0.1:0"
bootstrap = ["127.0.0.1:$dhtPort"]
discover = "tcp-echo"
require_signed = true
trusted_keys = ["$identityKey"]
"@ | Set-Content -Path $dhtUntrustedToml -Encoding UTF8

# A second server exists for the ban checks: banning the loopback address on the
# deployment under test would stop every other check from reaching it.
$banServerToml = Join-Path $Root 'ban-server.toml'
@"
[server]
bind_addr = "127.0.0.1"
bind_port = $banControlPort
auth_token = "$token"
ban_after_failures = 3
ban_seconds = 20

$commonSecurity
cert_file = "$certFile"
key_file = "$keyFile"

[identity]
enabled = true
require_identity = true
allowed_keys = ["$identityKey"]

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $banDashboardPort

[metrics]
enabled = true

[audit]
enabled = true
path = "$RootFwd/ban-audit.jsonl"
"@ | Set-Content -Path $banServerToml -Encoding UTF8

# A client whose identity key the ban server does not accept, and which retries
# quickly so the failure count is reached inside the check.
$banBadIdentity = "$RootFwd/ban-bad-identity.key"
$banBadClientToml = Join-Path $Root 'ban-bad-client.toml'
@"
[client]
server_addr = "127.0.0.1:$banControlPort"
auth_token = "$token"
reconnect_seconds = 1
max_reconnect_seconds = 1

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

[identity]
enabled = true
key_file = "$banBadIdentity"
"@ | Set-Content -Path $banBadClientToml -Encoding UTF8

# A separately started pair for the graceful shutdown checks. These need to signal a
# running server, so they get their own process rather than disturbing the deployment
# the other checks use.
$graceServerToml = Join-Path $Root 'grace-server.toml'
@"
[server]
bind_addr = "127.0.0.1"
bind_port = $graceControlPort
auth_token = "$token"
graceful_shutdown_seconds = 4

# The probes and the counters are read while this server is draining, so it needs a
# dashboard and the metrics endpoint.
[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $graceDashboardPort

[metrics]
enabled = true

[audit]
enabled = true
path = "$RootFwd/grace-audit.jsonl"
"@ | Set-Content -Path $graceServerToml -Encoding UTF8

$graceClientToml = Join-Path $Root 'grace-client.toml'
@"
[client]
server_addr = "127.0.0.1:$graceControlPort"
auth_token = "$token"
reconnect_seconds = 1
max_reconnect_seconds = 2

[[proxies]]
name = "grace-echo"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
remote_port = $graceProxyPort
"@ | Set-Content -Path $graceClientToml -Encoding UTF8

# The same server, reached by a client it does accept: this is what proves a ban
# covers a source and not only the client that earned it.
$banOkClientToml = Join-Path $Root 'ban-ok-client.toml'
@"
[client]
server_addr = "127.0.0.1:$banControlPort"
auth_token = "$token"
reconnect_seconds = 1
max_reconnect_seconds = 1

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

[identity]
enabled = true
key_file = "$identityFile"

[[proxies]]
name = "ban-echo"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
remote_port = $banProxyPort
"@ | Set-Content -Path $banOkClientToml -Encoding UTF8

Write-Step "validating the configuration"
foreach ($pair in @(
        @($serverExe, $serverToml, 'server'),
        @($clientExe, $clientToml, 'client'),
        @($clientExe, $visitorToml, 'visitor'))) {
    $verdict = Invoke-Binary -FilePath $pair[0] -Arguments @('-config', $pair[1], '-check')
    if ($verdict -notmatch 'is valid') { throw "the $($pair[2]) configuration did not validate: $verdict" }
}
Write-Host "   every configuration file is valid"

Write-Step "starting the server"
$serverLog = Join-Path $Root 'server.log'
$server = Start-Background -FilePath $serverExe -Arguments @('-config', $serverToml) -LogPath $serverLog -WorkingDirectory $Root
Wait-ForPort -Port $controlPort | Out-Null

Test-Check 'the server is ready' {
    $ready = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$dashboardPort/readyz")
    if ($ready -ne '200') { throw "readyz returned $ready" }
    return $true
}
Test-Check 'the server reports a post-quantum key agreement' {
    if ((Read-Log $serverLog) -notmatch 'post-quantum key agreement') { throw "the startup banner did not mention it" }
    return $true
}
Test-Check 'the server logs the TLS control port' {
    if ((Read-Log $serverLog) -notmatch 'wrapped in TLS') { throw "the startup banner did not mention TLS" }
    return $true
}
Test-Check 'the server logs the required identity' {
    if ((Read-Log $serverLog) -notmatch 'require_identity=True') { throw "the startup banner did not mention the identity policy" }
    return $true
}
Test-Check 'the server refuses a plain-text control connection' {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect('127.0.0.1', $controlPort)
        $client.ReceiveTimeout = 2000
        $stream = $client.GetStream()
        $probe = [byte[]](0x01, 0x00, 0x00, 0x00, 0x00, 0x02, 0x7b, 0x7d)
        $stream.Write($probe, 0, $probe.Length)
        Start-Sleep -Milliseconds 400
        $buffer = New-Object byte[] 16
        try {
            $count = $stream.Read($buffer, 0, $buffer.Length)
        } catch {
            $count = 0
        }
        # A TLS server answers with a handshake record, never with a protocol frame.
        if ($count -gt 0) {
            $looksLikeFrame = ($buffer[0] -eq 0x02)
            if ($looksLikeFrame) { throw "the plain-text probe received a protocol frame" }
        }
        return $true
    } finally {
        $client.Dispose()
    }
}

Write-Step "starting the owner client"
$clientLog = Join-Path $Root 'client.log'
$client = Start-Background -FilePath $clientExe -Arguments @('-config', $clientToml) -LogPath $clientLog -WorkingDirectory $Root
Start-Sleep -Seconds 3

Test-Check 'the client authenticated over TLS with an identity' {
    $log = Read-Log $clientLog
    if ($log -notmatch 'as session') { throw "no session was established: $log" }
    return $true
}
Test-Check 'the client agreed a post-quantum session key' {
    if ((Read-Log $clientLog) -notmatch 'post-quantum session key') { throw "the client did not report an agreed key" }
    return $true
}
Test-Check 'the server registered every proxy' {
    $confirmed = Read-Log $clientLog
    foreach ($name in @('tcp-echo', 'udp-echo', 'web', 'secure-web', 'private', 'private-udp', 'direct')) {
        if ($confirmed -notmatch [regex]::Escape($name)) { throw "proxy $name was never mentioned" }
    }
    return $true
}

Write-Step "exercising the proxy types"
Start-Sleep -Seconds 1

Test-Check 'tcp: a byte stream through the published port' {
    $reply = Invoke-TcpEcho -Port $tcpProxyPort -Payload 'tcp-through-the-tunnel'
    if ($reply -ne 'tcp-through-the-tunnel') { throw "echo returned '$reply'" }
    return $true
}

Test-Check 'udp: datagrams through the published port' {
    $reply = Invoke-UdpEcho -Port $udpProxyPort -Payload 'udp-through-the-tunnel'
    if ($reply -ne 'udp-through-the-tunnel') { throw "echo returned '$reply'" }
    return $true
}

Test-Check 'the udp session gauge counts a session while it is open' {
    # The gauge has to be read while a datagram proxy is published: a datagram sent to a
    # port nobody holds is answered with an ICMP unreachable, which is a different
    # failure from the one being checked.
    $before = Get-Metric 'aethertunnel_udp_sessions_active'
    # A datagram from a source port this run has not used yet is a new session.
    $socket = New-Object System.Net.Sockets.UdpClient
    try {
        $socket.Client.ReceiveTimeout = 4000
        $socket.Connect('127.0.0.1', $udpProxyPort)
        $bytes = [System.Text.Encoding]::ASCII.GetBytes('gauge-session')
        $socket.Send($bytes, $bytes.Length) | Out-Null
        $remote = New-Object System.Net.IPEndPoint([System.Net.IPAddress]::Any, 0)
        $reply = $socket.Receive([ref]$remote)
        if ([System.Text.Encoding]::ASCII.GetString($reply) -ne 'gauge-session') {
            throw "the udp proxy answered '$([System.Text.Encoding]::ASCII.GetString($reply))'"
        }

        $deadline = (Get-Date).AddSeconds(10)
        while ($true) {
            $now = Get-Metric 'aethertunnel_udp_sessions_active'
            if ($now -gt $before) { return $true }
            if ((Get-Date) -gt $deadline) { throw "the gauge stayed at $now although a new session was opened (was $before)" }
            Start-Sleep -Milliseconds 200
        }
    } finally {
        $socket.Dispose()
    }
}

Test-Check 'http: virtual hosting selects the tunnel by Host header' {
    $body = Invoke-Curl @('-s', '-H', 'Host: web.smoke.test', "http://127.0.0.1:$httpPort/hello")
    if ($body -notmatch 'smoketest-http') { throw "unexpected body: $body" }
    if ($body -notmatch 'host=web\.smoke\.test') { throw "the Host header did not survive: $body" }
    if ($body -notmatch 'path=/hello') { throw "the path did not survive: $body" }
    return $true
}

Test-Check 'http: an unknown host is refused' {
    $status = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '-H', 'Host: nobody.smoke.test', "http://127.0.0.1:$httpPort/")
    if ($status -ne '404') { throw "status was $status" }
    return $true
}

Test-Check 'https: virtual hosting over TLS' {
    $body = Invoke-Curl @('-sk', '-H', 'Host: secure.smoke.test', "https://127.0.0.1:$httpsPort/secure")
    if ($body -notmatch 'host=secure\.smoke\.test') { throw "unexpected body: $body" }
    return $true
}

Test-Check 'an http proxy without domains is reachable through subdomain_host' {
    # The proxy named subdomain-web declares no domains, so the only way to reach it
    # is <proxy-name>.<server.subdomain_host>. The client that published it cannot
    # check this itself: subdomain_host belongs to the server.
    $body = Invoke-Curl @('-s', '-H', 'Host: subdomain-web.smoke.test', "http://127.0.0.1:$httpPort/by-subdomain")
    if ($body -notmatch 'host=subdomain-web\.smoke\.test') { throw "unexpected body: $body" }
    if ($body -notmatch 'path=/by-subdomain') { throw "the request did not reach the local service: $body" }
    return $true
}

Test-Check 'a subdomain that was never published is refused' {
    $status = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '-H', 'Host: nobody.smoke.test', "http://127.0.0.1:$httpPort/")
    if ($status -ne '404') { throw "an unknown subdomain answered $status" }
    $status = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', '-H', 'Host: subdomain-web.other.test', "http://127.0.0.1:$httpPort/")
    if ($status -ne '404') { throw "a name outside the subdomain host answered $status" }
    return $true
}

Test-Check 'the proxy list is reported to the client' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/proxies")
    if ($body -notmatch '"tcp-echo"') { throw "the dashboard does not list tcp-echo: $body" }
    if ($body -notmatch '"udp"') { throw "the dashboard does not list a udp proxy" }
    if ($body -notmatch '"http"') { throw "the dashboard does not list an http proxy" }
    if ($body -notmatch '"xtcp"') { throw "the dashboard does not list an xtcp proxy" }
    return $true
}

Write-Step "starting the visitor client"
$visitorLog = Join-Path $Root 'visitor.log'
$visitor = Start-Background -FilePath $clientExe -Arguments @('-config', $visitorToml) -LogPath $visitorLog -WorkingDirectory $Root
Wait-ForPort -Port $stcpVisitorPort | Out-Null
Wait-ForPort -Port $xtcpVisitorPort | Out-Null
# The sudp listener is a datagram socket, so a TCP probe would never succeed.
Start-Sleep -Seconds 1

Test-Check 'stcp: a visitor reaches the private service' {
    $reply = Invoke-TcpEcho -Port $stcpVisitorPort -Payload 'stcp-through-the-visitor'
    if ($reply -ne 'stcp-through-the-visitor') { throw "echo returned '$reply'" }
    return $true
}

Test-Check 'stcp: the visitor proved knowledge of the secret' {
    $log = Read-Log $visitorLog
    if ($log -match 'invalid secret key') { throw "the server refused the visitor's secret" }
    return $true
}

Test-Check 'sudp: a visitor relays datagrams' {
    $reply = Invoke-UdpEcho -Port $sudpVisitorPort -Payload 'sudp-through-the-visitor'
    if ($reply -ne 'sudp-through-the-visitor') { throw "echo returned '$reply'" }
    return $true
}

Test-Check 'xtcp: a visitor either punches directly or relays' {
    $reply = Invoke-TcpEcho -Port $xtcpVisitorPort -Payload 'xtcp-through-the-visitor'
    if ($reply -ne 'xtcp-through-the-visitor') { throw "echo returned '$reply'" }
    return $true
}

Test-Check 'xtcp: the visitor reported which path it took' {
    $log = Read-Log $visitorLog
    if ($log -notmatch 'direct path to|falling back to the relayed path') {
        throw "the visitor did not report a path: $log"
    }
    return $true
}

Test-Check 'xtcp: the owner reported the punch outcome' {
    $log = Read-Log $clientLog
    if ($log -notmatch 'punch for "direct" succeeded|the visitor will use the relay') {
        throw "the owner did not report a punch outcome"
    }
    return $true
}

Test-Check 'idle_timeout_seconds: a silent visiting connection is dropped' {
    # The visitor client is configured with idle_timeout_seconds = 2. One echo round
    # trip proves the session works, and then nothing is sent: the connection has to
    # end on its own, which is the difference between a timeout and a connection that
    # merely failed to open.
    $probe = New-Object System.Net.Sockets.TcpClient('127.0.0.1', $stcpVisitorPort)
    try {
        $stream = $probe.GetStream()
        $bytes = [System.Text.Encoding]::ASCII.GetBytes('before-going-idle')
        $stream.Write($bytes, 0, $bytes.Length)
        $buffer = New-Object byte[] 64
        $read = $stream.Read($buffer, 0, $buffer.Length)
        $answer = [System.Text.Encoding]::ASCII.GetString($buffer, 0, $read)
        if ($answer -ne 'before-going-idle') { throw "the session answered '$answer' before going idle" }

        # A read that times out raises; a read that returns zero means the far end
        # closed. Either way what matters is when the wait ended.
        $stream.ReadTimeout = 8000
        $silentSince = Get-Date
        try {
            $read = $stream.Read($buffer, 0, $buffer.Length)
            if ($read -ne 0) { throw "the idle session carried $read byte(s) of its own" }
        } catch {
            if ($_.Exception.Message -like '*of its own*') { throw }
        }
        $took = ((Get-Date) - $silentSince).TotalSeconds
    } finally {
        $probe.Dispose()
    }
    if ($took -gt 6) {
        throw "the session was still open $([math]::Round($took, 1))s into the silence, although idle_timeout_seconds is 2"
    }
    if ($took -lt 1) {
        throw "the session ended $([math]::Round($took, 1))s into the silence, too soon to be the 2 second idle timeout"
    }
    return $true
}

Test-Check 'idle_timeout_seconds: a busy visiting connection is kept' {
    # The check above would also pass if the session were capped at two seconds
    # outright. Keeping bytes moving across that boundary is what makes the setting a
    # timeout on silence rather than on age.
    $probe = New-Object System.Net.Sockets.TcpClient('127.0.0.1', $stcpVisitorPort)
    try {
        $stream = $probe.GetStream()
        $stream.ReadTimeout = 5000
        $buffer = New-Object byte[] 64
        $started = Get-Date
        $rounds = 0
        while (((Get-Date) - $started).TotalSeconds -lt 5) {
            $text = "round-$rounds"
            $bytes = [System.Text.Encoding]::ASCII.GetBytes($text)
            $stream.Write($bytes, 0, $bytes.Length)
            $read = $stream.Read($buffer, 0, $buffer.Length)
            $answer = [System.Text.Encoding]::ASCII.GetString($buffer, 0, $read)
            if ($answer -ne $text) { throw "round $rounds answered '$answer'" }
            $rounds++
        }
    } finally {
        $probe.Dispose()
    }
    $kept = ((Get-Date) - $started).TotalSeconds
    if ($rounds -lt 3) { throw "only $rounds round trip(s) got through in $([math]::Round($kept, 1))s" }
    return $true
}

Write-Step "exercising the socks5 exit and the per-proxy ACL"

Test-Check 'socks5: a visitor reaches the address it asks for' {
    # The visitor names the target; the client dials it through its own network,
    # so the address below is one only this host can reach.
    $result = Invoke-CurlExit @('-s', '--max-time', '15', '--socks5-hostname', "127.0.0.1:$socksPort",
        "http://127.0.0.1:$httpEchoPort/through-socks")
    if ($result.Exit -ne 0) { throw "curl exited $($result.Exit): $($result.Output)" }
    if ($result.Output -notmatch 'smoketest-http') { throw "unexpected body: $($result.Output)" }
    if ($result.Output -notmatch 'path=/through-socks') { throw "the path did not survive: $($result.Output)" }
    return $true
}

Test-Check 'socks5: a target outside allow_targets is refused' {
    $result = Invoke-CurlExit @('-s', '--max-time', '15', '--socks5-hostname', "127.0.0.1:$socksPort",
        'http://10.99.0.1/')
    if ($result.Exit -eq 0) { throw "a target outside the allow list was reached: $($result.Output)" }
    return $true
}

Test-Check 'socks5: the refusal names the target and is logged' {
    $log = Read-Log $clientLog
    if ($log -notmatch 'socks: target 10\.99\.0\.1:80 is not allowed') {
        throw "the client did not report the refused target: $log"
    }
    return $true
}

Test-Check 'socks5: the dashboard reports the exit with its traffic' {
    $body = Invoke-Curl @('-s', '-H', "Authorization: Bearer $token", "http://127.0.0.1:$dashboardPort/api/proxies")
    $proxy = ($body | ConvertFrom-Json).proxies | Where-Object { $_.name -eq 'socks-exit' }
    if (-not $proxy) { throw "the exit is not reported: $body" }
    if ($proxy.type -ne 'socks5') { throw "the exit is reported as type '$($proxy.type)'" }
    if ([int]$proxy.total_connections -lt 1) { throw "the exit reports $($proxy.total_connections) connections" }
    if ([int64]$proxy.bytes_in -le 0) { throw "the exit reports $($proxy.bytes_in) bytes in" }
    return $true
}

Test-Check 'per-proxy acl: a visitor outside allow_cidrs is refused' {
    # The connection is admitted by the server and refused by the proxy's own list,
    # so the visitor sees the stream close without an answer.
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect('127.0.0.1', $aclProxyPort)
        $client.ReceiveTimeout = 3000
        $stream = $client.GetStream()
        $bytes = [System.Text.Encoding]::ASCII.GetBytes('acl-should-not-arrive')
        $stream.Write($bytes, 0, $bytes.Length)
        $buffer = New-Object byte[] 64
        try { $count = $stream.Read($buffer, 0, $buffer.Length) } catch { $count = 0 }
        if ($count -gt 0) { throw "the proxy served a visitor it does not allow: $([System.Text.Encoding]::ASCII.GetString($buffer, 0, $count))" }
        return $true
    } finally {
        $client.Dispose()
    }
}

Test-Check 'per-proxy acl: the refusal is recorded with the proxy name' {
    $audit = Get-Content -Raw (Join-Path $Root 'audit.jsonl')
    if ($audit -notmatch 'proxy_visitor_denied') { throw "no proxy visitor refusal is recorded" }
    if ($audit -notmatch '"proxy":"acl-echo"') { throw "the refusal does not name the proxy" }
    # The same refusal is counted, so an operator can graph it without reading the log.
    $denied = Get-Metric 'aethertunnel_visitors_denied_by_proxy_total'
    if ($denied -lt 1) { throw "the proxy visitor refusal counter is $denied after a refusal was recorded" }
    return $true
}

Test-Check 'per-proxy acl: a visitor inside allow_cidrs is served' {
    if (-not (Test-SecondLoopback)) {
        # The proxy allows 127.0.0.2 only, so the accept path needs that address.
        Write-Host "   skipped: this platform answers only on 127.0.0.1, so a second visitor address is not available"
        return $true
    }
    # The second loopback address is the one the proxy allows.
    $reply = Invoke-TcpEcho -Port $aclProxyPort -Payload 'acl-allowed' -LocalAddress '127.0.0.2'
    if ($reply -ne 'acl-allowed') { throw "echo returned '$reply'" }
    return $true
}

Test-Check 'the clients view counts the streams the owner session carried' {
    # Every check above put a stream through the owner client, so its session has to
    # report them; the totals used to be constants that were never written. Some
    # streams are still open on purpose at this point: a udp or sudp session lives
    # per visitor address, and the reverse proxy keeps its tunneled connection for the
    # next request, so only the completed count and its relation to the active one are
    # asserted here.
    $clients = @((Invoke-Curl @('-s', '-H', "Authorization: Bearer $token", "http://127.0.0.1:$dashboardPort/api/clients") | ConvertFrom-Json).clients)
    if ($clients.Count -lt 1) { throw "the clients view is empty while two clients are connected" }

    $total = 0
    $active = 0
    foreach ($client in $clients) {
        $total += [int]$client.total_streams
        $active += [int]$client.active_streams
    }
    if ($total -lt 1) { throw "every client reports 0 completed streams although the checks above ran dozens" }
    if ($active -gt $total) { throw "$active stream(s) are active out of $total completed" }
    Write-Host "   the sessions report $total completed stream(s), $active still open"
    return $true
}

Write-Step "checking the refusal paths"

$badVisitorLog = Join-Path $Root 'bad-visitor.log'
$badVisitor = Start-Background -FilePath $clientExe -Arguments @('-config', $badVisitorToml) -LogPath $badVisitorLog -WorkingDirectory $Root
# The listen socket is a datagram socket, so a TCP probe would never succeed.
Start-Sleep -Seconds 2

Test-Check 'a visitor with the wrong secret cannot reach the service' {
    try {
        $reply = Invoke-UdpEcho -Port $badVisitorPort -Payload 'should-not-arrive' -TimeoutMs 1500
        throw "the wrong secret was accepted: '$reply'"
    } catch {
        if ($_.Exception.Message -like '*the wrong secret was accepted*') { throw }
        return $true
    }
}

Test-Check 'the server recorded the refusal of the wrong secret' {
    $audit = Get-Content -Raw (Join-Path $Root 'audit.jsonl')
    if ($audit -notmatch 'invalid secret key') { throw "no refusal of a wrong secret is recorded" }
    return $true
}

if ($badVisitor -and -not $badVisitor.HasExited) { Stop-Process -Id $badVisitor.Id -Force -ErrorAction SilentlyContinue }

Write-Step "checking the directory and the ledger"

Test-Check 'the DHT node is serving' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/dht")
    $dht = $body | ConvertFrom-Json
    if (-not $dht.enabled) { throw "the DHT is not enabled: $body" }
    if (-not $dht.node_id -or $dht.node_id.Length -ne 40) { throw "the node id is '$($dht.node_id)'" }
    if ($dht.advertise_as -ne '127.0.0.1') { throw "the node advertises as '$($dht.advertise_as)'" }
    return $true
}

Test-Check 'the DHT announced every public proxy' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/dht")
    $announced = ($body | ConvertFrom-Json).announced
    foreach ($name in @('tcp-echo', 'udp-echo', 'private', 'direct', 'socks-exit')) {
        if ($announced -notcontains $name) { throw "$name was not announced; the directory holds $($announced -join ', ')" }
    }
    return $true
}

Test-Check 'the DHT publishes its announcement signing key' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/dht")
    $key = ($body | ConvertFrom-Json).signing_key
    if ($key -ne $dhtKey) { throw "the dashboard reports signing key '$key', want '$dhtKey'" }
    return $true
}

Test-Check 'a lookup resolves a proxy name to the port it is published on' {
    # The server's own configuration, while that server is running: the query node
    # binds its own port, so the lookup still works on the same host.
    $record = (Invoke-Binary -FilePath $serverExe -Arguments @('-config', $serverToml, '-dht-lookup', 'tcp-echo') -AllowFailure).Trim()
    if ($record -notmatch "-> 127\.0\.0\.1:$tcpProxyPort") { throw "the lookup returned '$record'" }
    return $true
}

Test-Check 'a client-role process resolves a proxy name through the DHT' {
    $resolved = (Invoke-Binary -FilePath $clientExe -Arguments @('-config', $dhtToml, '-discover', 'tcp-echo') -AllowFailure).Trim()
    if ($resolved -notmatch "-> 127\.0\.0\.1:$tcpProxyPort") { throw "discovery returned '$resolved'" }
    return $true
}

Test-Check 'a proxy name inside a private tunnel resolves as well' {
    $resolved = (Invoke-Binary -FilePath $clientExe -Arguments @('-config', $dhtToml, '-discover', 'private') -AllowFailure).Trim()
    if ($resolved -notmatch [regex]::Escape("-> 127.0.0.1:$controlPort")) { throw "discovery returned '$resolved'" }
    if ($resolved -notmatch [regex]::Escape('(type stcp')) { throw "the record does not report the private type: '$resolved'" }
    return $true
}

Test-Check 'a name that was never published is reported as unknown' {
    $output = Invoke-Binary -FilePath $clientExe -Arguments @('-config', $dhtToml, '-discover', 'nothing-here') -AllowFailure
    if ($output -notmatch 'not found') { throw "the lookup returned '$output'" }
    if ($output -match 'not found' -and $output -notmatch 'failed') { throw "the lookup did not fail: $output" }
    return $true
}

Test-Check 'a lookup reports which key signed the announcement' {
    $record = (Invoke-Binary -FilePath $serverExe -Arguments @('-config', $serverToml, '-dht-lookup', 'tcp-echo') -AllowFailure).Trim()
    if ($record -notmatch [regex]::Escape("signed by $dhtKey")) { throw "the lookup returned '$record'" }
    return $true
}

Test-Check 'a reader that trusts the announcement key resolves the name' {
    $resolved = (Invoke-Binary -FilePath $clientExe -Arguments @('-config', $dhtTrustedToml, '-discover', 'tcp-echo') -AllowFailure).Trim()
    if ($resolved -notmatch "-> 127\.0\.0\.1:$tcpProxyPort") { throw "discovery returned '$resolved'" }
    if ($resolved -notmatch 'signed by') { throw "the reader did not report a verified signature: '$resolved'" }
    return $true
}

Test-Check 'a reader that trusts another key refuses the announcement' {
    $output = Invoke-Binary -FilePath $clientExe -Arguments @('-config', $dhtUntrustedToml, '-discover', 'tcp-echo') -AllowFailure
    if ($output -notmatch 'not trusted') { throw "the reader accepted a record signed by another key: '$output'" }
    return $true
}

Test-Check 'the ledger has no entry while the client is still connected' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/ledger")
    $ledger = $body | ConvertFrom-Json
    if (-not $ledger.enabled) { throw "the ledger is not enabled: $body" }
    if ($ledger.count -ne 0) { throw "the ledger already holds $($ledger.count) entries before any client left" }
    if ($ledger.public_key.Length -ne 64) { throw "the public key is '$($ledger.public_key)'" }
    return $true
}

Write-Step "checking the proxy pool, the multipath spread and the punch outcome"

# Two separate clients publish the same proxy name on the same port with the same
# group, which is what makes the server pool them behind one endpoint. Both serve the
# same echo, so the members are told apart by their own counters in /api/proxies
# rather than by what they answer.
$poolAToml = Join-Path $Root 'pool-a.toml'
$poolBToml = Join-Path $Root 'pool-b.toml'
@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

$commonObfuscation

[identity]
enabled = true
key_file = "$identityFile"

[[proxies]]
name = "pooled"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
remote_port = $poolProxyPort
group = "smoke-pool"

# A datagram proxy with multipath opens this many data connections for one visitor
# address, so one session shows up as several connections on the server.
[[proxies]]
name = "spread"
type = "udp"
local_ip = "127.0.0.1"
local_port = $udpEchoPort
remote_port = $spreadProxyPort
multipath = 3
"@ | Set-Content -Path $poolAToml -Encoding UTF8

@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

$commonObfuscation

[identity]
enabled = true
key_file = "$identityFile"

[[proxies]]
name = "pooled"
type = "tcp"
local_ip = "127.0.0.1"
local_port = $tcpEchoPort
remote_port = $poolProxyPort
group = "smoke-pool"
"@ | Set-Content -Path $poolBToml -Encoding UTF8

$poolALog = Join-Path $Root 'pool-a.log'
$poolBLog = Join-Path $Root 'pool-b.log'
$poolA = Start-Background -FilePath $clientExe -Arguments @('-config', $poolAToml) -LogPath $poolALog -WorkingDirectory $Root
$poolB = Start-Background -FilePath $clientExe -Arguments @('-config', $poolBToml) -LogPath $poolBLog -WorkingDirectory $Root
Wait-ForPort -Port $poolProxyPort | Out-Null
Start-Sleep -Seconds 1

# The members of a pool, as the dashboard reports them.
function Get-PoolMembers {
    $proxies = @((Invoke-Curl @('-s', '-H', "Authorization: Bearer $token", "http://127.0.0.1:$dashboardPort/api/proxies") | ConvertFrom-Json).proxies)
    $pooled = $proxies | Where-Object { $_.name -eq 'pooled' } | Select-Object -First 1
    if (-not $pooled) { throw "the pooled proxy is not published" }
    return @($pooled.members)
}

Test-Check 'two clients publishing the same name share one endpoint' {
    $members = Get-PoolMembers
    if ($members.Count -ne 2) { throw "the pool has $($members.Count) member(s), want 2" }
    return $true
}

Test-Check 'the load balancer spreads requests over both members' {
    # Round-robin is the default strategy, so six requests have to reach both
    # members. Every request is checked, not just counted: a pool that answered with
    # an error would otherwise look busy.
    $deadline = (Get-Date).AddSeconds(10)
    while ($true) {
        for ($attempt = 0; $attempt -lt 6; $attempt++) {
            $reply = Invoke-TcpEcho -Port $poolProxyPort -Payload 'pooled-request'
            if ($reply -ne 'pooled-request') { throw "the pool answered '$reply'" }
        }
        $members = Get-PoolMembers
        $served = @($members | Where-Object { [int]$_.total_connections -ge 1 })
        if ($served.Count -eq 2) { return $true }
        if ((Get-Date) -gt $deadline) {
            throw "only $($served.Count) of $($members.Count) members served a request: $(($members | ForEach-Object { $_.client_id + '=' + $_.total_connections }) -join ', ')"
        }
    }
}

Test-Check 'the multipath proxy opens several data connections for one session' {
    $before = Get-Metric 'aethertunnel_data_connections_total'
    $reply = Invoke-UdpEcho -Port $spreadProxyPort -Payload 'spread-over-three-paths'
    if ($reply -ne 'spread-over-three-paths') { throw "the multipath proxy answered '$reply'" }

    $deadline = (Get-Date).AddSeconds(10)
    while ($true) {
        $opened = (Get-Metric 'aethertunnel_data_connections_total') - $before
        if ($opened -ge 3) {
            Write-Host "   the session opened $opened data connection(s)"
            return $true
        }
        if ((Get-Date) -gt $deadline) { throw "one datagram session opened $opened data connection(s), want at least 3" }
        Start-Sleep -Milliseconds 200
    }
}

Test-Check 'the punch outcome in the metrics is the one the visitor reported' {
    # The visitor says which path it took in its own log. The server cannot see a
    # direct path at all: a visitor that goes direct simply stops using its control
    # connection, which is why it reports the path over the rendezvous socket. That
    # report travels over UDP, so the counters are given a moment to settle.
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Metric 'aethertunnel_p2p_direct_total') + (Get-Metric 'aethertunnel_p2p_relayed_total') -lt 1) {
        if ((Get-Date) -gt $deadline) { throw "no punch outcome was counted although an xtcp visitor connected" }
        Start-Sleep -Milliseconds 250
    }

    $log = Read-Log $visitorLog
    $direct = Get-Metric 'aethertunnel_p2p_direct_total'
    $relayed = Get-Metric 'aethertunnel_p2p_relayed_total'
    if ((Get-Metric 'aethertunnel_p2p_punches_total') -lt 1) { throw "no punch was counted although an xtcp visitor connected" }
    if ($log -match 'direct path to .* established') {
        if ($direct -lt 1) { throw "the visitor took the direct path but the server counted no direct outcome" }
    } elseif ($direct -ge 1) {
        throw "the server counted a direct outcome although the visitor reported the relayed path"
    }
    Write-Host "   direct $direct, relayed $relayed"
    return $true
}

Test-Check 'the punch outcome is recorded in the audit log' {
    $audit = Get-Content -Raw (Join-Path $Root 'audit.jsonl')
    $direct = $audit -match '"event":"p2p_direct"'
    $relayed = $audit -match '"event":"p2p_relayed"'
    $abandoned = $audit -match '"event":"p2p_abandoned"'
    if (-not ($direct -or $relayed -or $abandoned)) { throw "the audit log records no punch outcome" }
    if ($direct -and $abandoned) { throw "the same attempt is recorded as both direct and abandoned" }
    return $true
}

# Stopping the second member has to leave the pool serving: that is what the pool is
# for, and it is also where the removal is audited.
if ($poolB -and -not $poolB.HasExited) { Stop-Process -Id $poolB.Id -Force -ErrorAction SilentlyContinue }

Test-Check 'the pool keeps serving after one member leaves' {
    $deadline = (Get-Date).AddSeconds(15)
    while ($true) {
        $members = @(Get-PoolMembers)
        if ($members.Count -eq 1) {
            $reply = Invoke-TcpEcho -Port $poolProxyPort -Payload 'after-the-member-left'
            if ($reply -ne 'after-the-member-left') { throw "the remaining member answered '$reply'" }
            return $true
        }
        if ((Get-Date) -gt $deadline) { throw "the pool still reports $($members.Count) members" }
        Start-Sleep -Milliseconds 250
    }
}

Test-Check 'the removal of a proxy is recorded in the audit log' {
    # The member is removed from the group before the record is written, so the API
    # can report one member while the audit line is still on its way.
    $deadline = (Get-Date).AddSeconds(10)
    while ($true) {
        $audit = Get-Content -Raw (Join-Path $Root 'audit.jsonl')
        if ($audit -match '"event":"proxy_removed"' -and $audit -match '"proxy":"pooled"') { return $true }
        if ((Get-Date) -gt $deadline) { throw "the audit log has no proxy_removed record for the pooled proxy" }
        Start-Sleep -Milliseconds 250
    }
}

if ($poolA -and -not $poolA.HasExited) { Stop-Process -Id $poolA.Id -Force -ErrorAction SilentlyContinue }
Start-Sleep -Seconds 1

Write-Step "stopping the owner client so its usage is recorded"
if ($client -and -not $client.HasExited) { Stop-Process -Id $client.Id -Force -ErrorAction SilentlyContinue }
Start-Sleep -Seconds 2

Test-Check 'the ledger records the usage of the session that ended' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/ledger")
    $ledger = $body | ConvertFrom-Json
    if ($ledger.count -lt 1) { throw "the ledger still holds $($ledger.count) entries" }

    # One entry is written per proxy the client published, and the proxies that
    # carried no traffic are recorded with zero bytes, so the check totals them.
    $bytes = 0
    foreach ($client in $ledger.totals.PSObject.Properties) {
        $bytes += [int64]$client.Value.bytes_in + [int64]$client.Value.bytes_out
    }
    if ($bytes -le 0) { throw "the ledger holds $($ledger.count) entries that between them record no traffic" }
    return $true
}

Test-Check 'the ledger verifies against the published public key' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/ledger")
    $publicKey = ($body | ConvertFrom-Json).public_key
    $output = (Invoke-Binary -FilePath $serverExe -Arguments @(
            '-verify-ledger', "$RootFwd/ledger.jsonl", '-ledger-key', $publicKey)).Trim()
    if ($output -notmatch 'entries verified') { throw "verification printed '$output'" }
    return $true
}

Test-Check 'a tampered ledger does not verify' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/api/ledger")
    $publicKey = ($body | ConvertFrom-Json).public_key

    $tampered = Join-Path $Root 'ledger-tampered.jsonl'
    $lines = Get-Content (Join-Path $Root 'ledger.jsonl')
    $entry = $lines[0] | ConvertFrom-Json
    $entry.bytes_in = $entry.bytes_in + 1
    ($entry | ConvertTo-Json -Compress) | Set-Content -Path $tampered -Encoding UTF8

    $output = Invoke-Binary -FilePath $serverExe -Arguments @(
        '-verify-ledger', "$RootFwd/ledger-tampered.jsonl", '-ledger-key', $publicKey) -AllowFailure
    if ($output -notmatch 'verification failed') { throw "a tampered ledger was accepted: $output" }
    return $true
}

Test-Check 'a ledger verified against another key is refused' {
    $foreignKey = 'a' * 64
    $output = Invoke-Binary -FilePath $serverExe -Arguments @(
        '-verify-ledger', "$RootFwd/ledger.jsonl", '-ledger-key', $foreignKey) -AllowFailure
    if ($output -notmatch 'verification failed') { throw "a ledger verified under a foreign key: $output" }
    if ($output -notmatch 'does not verify') { throw "the failure does not name the signature: $output" }
    return $true
}

Test-Check 'the vpn section refuses to start where there is no tun device' {
    $vpnToml = Join-Path $Root 'vpn-server.toml'
    @"
[server]
bind_addr = "127.0.0.1"
bind_port = $vpnPort
auth_token = "$token"

[vpn]
enabled = true
device = "tun0"
address = "10.7.0.0/24"
"@ | Set-Content -Path $vpnToml -Encoding UTF8

    if ($env:OS -ne 'Windows_NT') {
        # On Linux the server would open a real tun interface and keep running, which
        # is not something this script drives. The tun path is covered by the package
        # tests, and by the -race job in CI.
        Write-Host "   skipped on this platform: a tun device cannot be opened non-interactively here"
        return $true
    }

    $output = Invoke-Binary -FilePath $serverExe -Arguments @('-config', $vpnToml) -AllowFailure
    if ($output -notmatch 'no tun implementation') { throw "the server did not report the missing tun device: $output" }
    if ($output -notmatch 'Wintun') { throw "the message does not say what a tun device on Windows needs: $output" }
    return $true
}

Write-Step "checking the operational surface"
Test-Check 'the audit log recorded the security events' {
    $auditPath = Join-Path $Root 'audit.jsonl'
    if (-not (Test-Path $auditPath)) { throw "no audit log was written" }
    $audit = Get-Content -Raw $auditPath
    foreach ($event in @('control_accepted', 'proxy_registered', 'visitor_accepted', 'visitor_rejected')) {
        if ($audit -notmatch $event) { throw "the audit log has no $event record" }
    }
    return $true
}

Test-Check 'the metrics endpoint is protected' {
    $status = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$dashboardPort/metrics")
    if ($status -ne '200') { throw "metrics returned $status without a token" }
    return $true
}

Test-Check 'the metrics endpoint reports the data path' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/metrics")
    foreach ($series in @(
            'aethertunnel_control_connections_total',
            'aethertunnel_control_rejected_total',
            'aethertunnel_data_connections_total',
            'aethertunnel_udp_datagrams_total',
            'aethertunnel_http_requests_total',
            'aethertunnel_p2p_punches_total',
            'aethertunnel_bytes_to_clients_total',
            'aethertunnel_socks5_requests_total',
            'aethertunnel_streams_refused_while_draining_total',
            'aethertunnel_tunnel_streams_active{tunnel="tcp-echo"}',
            'aethertunnel_tunnel_streams_total{tunnel="tcp-echo"}',
            'aethertunnel_tunnel_bytes_total{tunnel="tcp-echo",direction="from_client"}',
            'aethertunnel_tunnel_http_requests_total{tunnel="web"}',
            'aethertunnel_audit_write_failures_total',
            'aethertunnel_audit_records_lost_total',
            'aethertunnel_audit_records_recovered_total')) {
        if ($body -notmatch [regex]::Escape($series)) { throw "the metrics output has no $series" }
    }
    return $true
}

Test-Check 'the health endpoint is public and reports the running server' {
    # /api/health is deliberately readable without a token and is what a load balancer
    # or a monitoring agent polls, so it has to keep its shape and stay cheap.
    $health = Invoke-RestMethod -Uri "http://127.0.0.1:$dashboardPort/api/health" -TimeoutSec 10
    if ($health.status -ne 'ok') { throw "the health endpoint reports status '$($health.status)'" }
    if ($health.protocol -ne 4) { throw "the health endpoint reports protocol $($health.protocol)" }
    if (-not $health.version) { throw "the health endpoint reports no version" }
    if ($health.uptime_seconds -lt 1) { throw "the health endpoint reports uptime $($health.uptime_seconds)" }
    return $true
}

Test-Check 'the status endpoint carries every field the panel draws' {
    # The panel polls /api/status every two seconds and reads exactly these fields;
    # a renamed one leaves a dash on screen instead of a number.
    $status = Get-Status
    if (-not $status.version) { throw "the status endpoint reports no version" }
    # The binary is built here without ldflags, so the version is the source default;
    # what matters is that the dashboard and --version agree.
    $reported = (Invoke-Binary -FilePath $serverExe -Arguments @('-version')).Trim()
    if ($reported -notmatch [regex]::Escape($status.version)) {
        throw "the status endpoint reports '$($status.version)' while the binary reports '$reported'"
    }
    if ($status.protocol -ne 4) { throw "the status endpoint reports protocol $($status.protocol)" }
    if ($status.uptime_seconds -lt 1) { throw "the status endpoint reports uptime $($status.uptime_seconds)" }
    if (-not $status.started_at) { throw "the status endpoint reports no start time" }
    if ($null -eq $status.connections.active -or $null -eq $status.connections.total -or $null -eq $status.connections.max) {
        throw "the status endpoint reports connections $($status.connections | ConvertTo-Json -Compress)"
    }
    if ($status.connections.total -lt 1) { throw "the status endpoint counts $($status.connections.total) connections" }
    if ($null -eq $status.proxies.registered -or $null -eq $status.proxies.active_streams) {
        throw "the status endpoint reports proxies $($status.proxies | ConvertTo-Json -Compress)"
    }
    if ($null -eq $status.traffic.bytes_in -or $null -eq $status.traffic.bytes_out) {
        throw "the status endpoint reports traffic $($status.traffic | ConvertTo-Json -Compress)"
    }
    if ($status.traffic.bytes_in -le 0 -or $status.traffic.bytes_out -le 0) {
        throw "the status endpoint reports $($status.traffic.bytes_in)/$($status.traffic.bytes_out) bytes after the traffic above"
    }
    if ($null -eq $status.auth_required) { throw "the status endpoint does not say whether a token is required" }
    if ($status.audit.enabled -ne $true) { throw "the status endpoint reports the audit log as $($status.audit.enabled)" }
    if ($status.audit.writable -ne $true) { throw "the status endpoint reports the audit log as not writable" }
    if ($status.audit.records_lost -ne 0) { throw "the status endpoint reports $($status.audit.records_lost) lost audit records" }
    return $true
}

Test-Check 'the per-tunnel http counter names the tunnel that served the request' {
    # A labelled series is the one that answers "which tunnel is busy", so the label
    # has to match the proxy name rather than something else.
    $served = Get-LabelledMetric 'aethertunnel_tunnel_http_requests_total' 'tunnel="web"'
    if ($served -lt 1) { throw "the per-tunnel http counter for web is $served after the requests above" }
    $bytes = Get-LabelledMetric 'aethertunnel_tunnel_bytes_total' 'tunnel="tcp-echo",direction="from_client"'
    if ($bytes -lt 1) { throw "the per-tunnel byte counter for tcp-echo is $bytes after the streams above" }
    return $true
}

Test-Check 'the active stream count returns to zero when the streams end' {
    # Every check above opened and closed streams, and the client that carried them
    # has since disconnected. A gauge that only ever climbs is what the dashboard
    # would then show as "active conns", so it has to come back to zero.
    $deadline = (Get-Date).AddSeconds(6)
    while ($true) {
        # Invoke-Curl returns one string, so the metric lines are split out of it:
        # Select-String on a multi-line string would only ever match the first line.
        $metrics = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/metrics")
        $line = ($metrics -split "`n" | Where-Object { $_ -match '^aethertunnel_streams_active ' } | Select-Object -First 1)
        $active = if ($line) { [int](($line -split '\s+')[1]) } else { -1 }
        $proxies = @((Invoke-Curl @('-s', '-H', "Authorization: Bearer $token", "http://127.0.0.1:$dashboardPort/api/proxies") | ConvertFrom-Json).proxies)
        $busy = @($proxies | Where-Object { [int]$_.active_connections -ne 0 })
        if ($active -eq 0 -and $busy.Count -eq 0) { return $true }
        if ((Get-Date) -gt $deadline) {
            throw "after every stream finished: gauge=$active, still busy: $(($busy | ForEach-Object { $_.name }) -join ',')"
        }
        Start-Sleep -Milliseconds 250
    }
}

Test-Check 'the metrics counters moved' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/metrics")
    $match = [regex]::Match($body, 'aethertunnel_udp_datagrams_total (\d+)')
    if (-not $match.Success) { throw "no udp datagram counter" }
    if ([int]$match.Groups[1].Value -le 0) { throw "the udp datagram counter is still zero" }
    return $true
}

Write-Step "checking graceful shutdown under load"

# First scenario: a stream that finishes inside the grace period. The server has to
# keep carrying it while it winds down, and stop as soon as it ends.
$graceServerLog = Join-Path $Root 'grace-server.log'
$graceServer = Start-Background -FilePath $serverExe -Arguments @('-config', $graceServerToml) -LogPath $graceServerLog -WorkingDirectory $Root
Wait-ForPort -Port $graceControlPort | Out-Null
$graceClientLog = Join-Path $Root 'grace-client.log'
$graceClient = Start-Background -FilePath $clientExe -Arguments @('-config', $graceClientToml) -LogPath $graceClientLog -WorkingDirectory $Root
Wait-ForPort -Port $graceProxyPort | Out-Null

$held = Hold-Stream -Port $graceProxyPort -Payload 'before-the-signal'

Test-Check 'graceful shutdown: the server keeps a stream alive while it winds down' {
    Send-StopSignal $graceServer
    Start-Sleep -Milliseconds 800
    if ($graceServer.HasExited) {
        throw "the server exited at once although a stream was in flight and the grace period is 4s"
    }
    # The stream that was already running still carries its bytes.
    $bytes = [System.Text.Encoding]::ASCII.GetBytes('during-the-shutdown')
    $held.Stream.Write($bytes, 0, $bytes.Length)
    $buffer = New-Object byte[] 256
    $read = $held.Stream.Read($buffer, 0, $buffer.Length)
    $answer = [System.Text.Encoding]::ASCII.GetString($buffer, 0, $read)
    if ($answer -ne 'during-the-shutdown') { throw "the stream answered '$answer' during the shutdown" }
    return $true
}

Test-Check 'graceful shutdown: a visitor that arrives while the server drains is refused and counted' {
    # The published port still accepts connections while the server winds down: what
    # stops working is opening a stream behind it. That refusal is the one the
    # drain-refusal counter is for, and it was never observed moving.
    $late = New-Object System.Net.Sockets.TcpClient
    $late.ReceiveTimeout = 3000
    $served = $false
    try {
        $late.Connect('127.0.0.1', $graceProxyPort)
        $lateStream = $late.GetStream()
        $bytes = [System.Text.Encoding]::ASCII.GetBytes('late-visitor')
        $lateStream.Write($bytes, 0, $bytes.Length)
        $lateStream.Flush()
        $buffer = New-Object byte[] 64
        $read = $lateStream.Read($buffer, 0, $buffer.Length)
        $served = ($read -gt 0)
    } catch {
        $served = $false
    } finally {
        $late.Dispose()
    }
    if ($served) { throw "the visitor that arrived during the drain was served" }

    $count = 0
    $deadline = (Get-Date).AddSeconds(2)
    while ((Get-Date) -lt $deadline) {
        $count = Get-Metric 'aethertunnel_streams_refused_while_draining_total' -Port $graceDashboardPort
        if ($count -ge 1) { break }
        Start-Sleep -Milliseconds 100
    }
    if ($count -lt 1) { throw "the refusal was not counted: the drain counter is $count" }
    return $true
}

Test-Check 'graceful shutdown: /readyz says 503 while the server drains and /healthz stays 200' {
    # readyz is the one a load balancer reads: it has to stop routing here while the
    # streams already running are still being carried, and healthz has to keep saying
    # the process is alive, or a supervisor would restart it mid-drain.
    $ready = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$graceDashboardPort/readyz")
    if ($ready -ne '503') { throw "/readyz answered $ready while the server was draining" }
    $health = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$graceDashboardPort/healthz")
    if ($health -ne '200') { throw "/healthz answered $health while the server was draining" }
    return $true
}

Test-Check 'graceful shutdown: the server stops as soon as the last stream ends' {
    $held.Client.Dispose()
    $deadline = (Get-Date).AddSeconds(4)
    while (-not $graceServer.HasExited -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 100 }
    if (-not $graceServer.HasExited) {
        throw "the server was still running 4s after the last stream ended"
    }
    $log = Read-Log $graceServerLog
    if ($log -notmatch 'shutting down:') { throw "the server did not report a shutdown: $log" }
    if ($log -notmatch 'every stream finished within') {
        throw "the log does not report a drained shutdown: $log"
    }
    return $true
}

if ($graceClient -and -not $graceClient.HasExited) { Stop-Process -Id $graceClient.Id -Force -ErrorAction SilentlyContinue }
Start-Sleep -Seconds 1

# Second scenario: a stream that outlives the grace period. The wait has to be
# bounded, and the stream is disconnected once it runs out.
$graceServer2Log = Join-Path $Root 'grace-server2.log'
$graceServer2 = Start-Background -FilePath $serverExe -Arguments @('-config', $graceServerToml) -LogPath $graceServer2Log -WorkingDirectory $Root
Wait-ForPort -Port $graceControlPort | Out-Null
$graceClient2 = Start-Background -FilePath $clientExe -Arguments @('-config', $graceClientToml) -LogPath (Join-Path $Root 'grace-client2.log') -WorkingDirectory $Root
Wait-ForPort -Port $graceProxyPort | Out-Null

$stuck = Hold-Stream -Port $graceProxyPort -Payload 'stuck-stream'
$signalAt = Get-Date
Send-StopSignal $graceServer2

Test-Check 'graceful shutdown: a stream that outlives the grace period is disconnected' {
    Start-Sleep -Seconds 2
    if ($graceServer2.HasExited) {
        throw "the server exited 2s after the signal although its grace period is 4s"
    }
    $deadline = (Get-Date).AddSeconds(6)
    while (-not $graceServer2.HasExited -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 100 }
    if (-not $graceServer2.HasExited) { throw "the server was still running 6s after a 4s grace period" }

    $took = ((Get-Date) - $signalAt).TotalSeconds
    if ($took -lt 3.5 -or $took -gt 8) {
        throw "the server stopped $([math]::Round($took, 1))s after the signal, want about 4s"
    }

    $log = Read-Log $graceServer2Log
    if ($log -notmatch 'stream\(s\) were still running after') {
        throw "the log does not report a stream that outlived the grace period: $log"
    }

    # Giving up means disconnecting, so the stream the visitor was holding ends.
    try {
        $stuck.Stream.ReadTimeout = 3000
        $read = $stuck.Stream.Read((New-Object byte[] 32), 0, 32)
        if ($read -gt 0) { throw "the stream still carried $read byte(s) after the grace period" }
    } catch {
        if ($_.Exception.Message -like '*still carried*') { throw }
    }
    $stuck.Client.Dispose()
    return $true
}

if ($graceClient2 -and -not $graceClient2.HasExited) { Stop-Process -Id $graceClient2.Id -Force -ErrorAction SilentlyContinue }
if ($graceServer2 -and -not $graceServer2.HasExited) { Stop-Process -Id $graceServer2.Id -Force -ErrorAction SilentlyContinue }

Write-Step "checking the automatic ban of failing sources"

# A second server, so that banning the loopback address cannot disturb the checks
# above. It requires an identity and accepts only the owner client's key, which is
# how the failing client below is made to fail: it presents another key.
$banServerLog = Join-Path $Root 'ban-server.log'
$banServer = Start-Background -FilePath $serverExe -Arguments @('-config', $banServerToml) -LogPath $banServerLog -WorkingDirectory $Root
Wait-ForPort -Port $banControlPort | Out-Null

$banBadClientLog = Join-Path $Root 'ban-bad-client.log'
$banBadClient = Start-Background -FilePath $clientExe -Arguments @('-config', $banBadClientToml) -LogPath $banBadClientLog -WorkingDirectory $Root
# Three failures are needed; the client retries every second with the fast
# reconnect settings in its own configuration.
Start-Sleep -Seconds 8

Test-Check 'a source that keeps failing authentication is banned' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$banDashboardPort/metrics")
    $match = [regex]::Match($body, 'aethertunnel_sources_banned_total (\d+)')
    if (-not $match.Success) { throw "no banned-sources counter" }
    if ([int]$match.Groups[1].Value -lt 1) { throw "the counter is still $($match.Groups[1].Value) after three failures" }
    $failures = [regex]::Match($body, 'aethertunnel_auth_failures_total (\d+)')
    if (-not $failures.Success -or [int]$failures.Groups[1].Value -lt 3) {
        throw "the authentication failure counter is $($failures.Groups[1].Value)"
    }
    return $true
}

Test-Check 'the ban appears in the server log and the audit log' {
    $log = Read-Log $banServerLog
    if ($log -notmatch 'banned') { throw "the server never reported a ban: $log" }
    $audit = Get-Content -Raw (Join-Path $Root 'ban-audit.jsonl')
    if ($audit -notmatch 'source_banned') { throw "the audit log has no source_banned record" }
    return $true
}

if ($banBadClient -and -not $banBadClient.HasExited) { Stop-Process -Id $banBadClient.Id -Force -ErrorAction SilentlyContinue }

$banOkClientLog = Join-Path $Root 'ban-ok-client.log'
$banOkClient = Start-Background -FilePath $clientExe -Arguments @('-config', $banOkClientToml) -LogPath $banOkClientLog -WorkingDirectory $Root
Start-Sleep -Seconds 4

Test-Check 'a banned source is refused before the handshake' {
    $audit = Get-Content -Raw (Join-Path $Root 'ban-audit.jsonl')
    if ($audit -notmatch 'ban_refused') { throw "no refusal of a banned source is recorded" }
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$banDashboardPort/metrics")
    $match = [regex]::Match($body, 'aethertunnel_banned_connections_refused_total (\d+)')
    if (-not $match.Success -or [int]$match.Groups[1].Value -lt 1) {
        throw "the banned-connection counter is $($match.Groups[1].Value)"
    }
    return $true
}

Test-Check 'the ban covers the source, not the client that earned it' {
    # This client presents a key the server accepts and retries every second, so a
    # working path shows up in about a second. Three seconds without a session, well
    # inside the 20-second window the failing client earned, is the ban and not a
    # slow handshake.
    $deadline = (Get-Date).AddSeconds(3)
    while ((Get-Date) -lt $deadline) {
        if ((Read-Log $banOkClientLog) -match 'as session') {
            throw "a client with an accepted identity connected from a banned address: $(Read-Log $banOkClientLog)"
        }
        Start-Sleep -Milliseconds 250
    }
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$banDashboardPort/metrics")
    $accepted = [regex]::Match($body, 'aethertunnel_control_connections_total (\d+)')
    if ($accepted.Success -and [int]$accepted.Groups[1].Value -gt 0) {
        # Nothing has authenticated on this server: the failing client never got past
        # its identity check, and the accepted one has been refused since it started.
        throw "the server accepted $($accepted.Groups[1].Value) control connections from a banned source"
    }
    return $true
}

Test-Check 'the ban lapses after ban_seconds' {
    # ban_seconds is 20 in this configuration, and the accepted client reconnects
    # every second, so it holds a session once the entry expires.
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        if ((Read-Log $banOkClientLog) -match 'as session') { return $true }
        Start-Sleep -Milliseconds 500
    }
    throw "the accepted client was still refused 30s after the ban window: $(Read-Log $banOkClientLog)"
}

if ($banOkClient -and -not $banOkClient.HasExited) { Stop-Process -Id $banOkClient.Id -Force -ErrorAction SilentlyContinue }
if ($banServer -and -not $banServer.HasExited) { Stop-Process -Id $banServer.Id -Force -ErrorAction SilentlyContinue }

Write-Step "checking the server-wide access controls"

# A server of its own, because both of these rules are decided before any client is
# allowed in and would otherwise interfere with the checks above: deny_cidrs refuses
# a source outright, and the rate limit starves it for as long as it keeps
# connecting.
$guardControlPort = Get-FreePort
$guardDashboardPort = Get-FreePort
$guardAudit = Join-Path $Root 'guard-audit.jsonl'
$guardToml = Join-Path $Root 'guard.toml'
@"
[server]
bind_addr = "127.0.0.1"
bind_port = $guardControlPort
auth_token = "$token"
deny_cidrs = ["127.0.0.2/32"]
rate_limit_per_second = 1
rate_limit_burst = 2

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $guardDashboardPort

[metrics]
enabled = true

[audit]
enabled = true
path = "$RootFwd/guard-audit.jsonl"
"@ | Set-Content -Path $guardToml -Encoding UTF8

$guardLog = Join-Path $Root 'guard.log'
$guard = Start-Background -FilePath $serverExe -Arguments @('-config', $guardToml) -LogPath $guardLog -WorkingDirectory $Root
Wait-ForPort -Port $guardControlPort | Out-Null
Start-Sleep -Seconds 1

Test-Check 'a denied source is refused before the handshake' {
    if (-not (Test-SecondLoopback)) {
        Write-Host "   skipped: this platform answers only on 127.0.0.1, so a denied second source is not available"
        return $true
    }
    for ($attempt = 0; $attempt -lt 3; $attempt++) {
        Attempt-ControlConnection -Port $guardControlPort -From '127.0.0.2' | Out-Null
    }
    $deadline = (Get-Date).AddSeconds(10)
    while ($true) {
        $denied = Get-Metric 'aethertunnel_connections_denied_by_acl_total' -Port $guardDashboardPort
        if ($denied -ge 3) { break }
        if ((Get-Date) -gt $deadline) { throw "three connections from a denied source produced $denied acl refusals" }
        Start-Sleep -Milliseconds 200
    }
    if ((Get-Metric 'aethertunnel_control_connections_total' -Port $guardDashboardPort) -ne 0) {
        throw "a denied source was counted as an accepted control connection"
    }
    # Refusals are also counted together, which is the series an alert watches when it
    # does not care why a connection was turned away.
    $rejected = Get-Metric 'aethertunnel_control_rejected_total' -Port $guardDashboardPort
    if ($rejected -lt 3) { throw "three refusals left the rejection counter at $rejected" }
    if ($rejected -lt $denied) { throw "the rejection counter ($rejected) is below the acl counter ($denied)" }
    $audit = Get-Content -Raw $guardAudit
    if ($audit -notmatch '"event":"acl_denied"') { throw "the refusal was not audited" }
    return $true
}

Test-Check 'a source inside the burst is served and then rate limited' {
    # burst is 2 and the rate is one connection per second, so the first two
    # attempts are let through to the handshake and the rest are refused.
    for ($attempt = 1; $attempt -le 2; $attempt++) {
        Attempt-ControlConnection -Port $guardControlPort | Out-Null
    }
    Start-Sleep -Milliseconds 300
    if ((Get-Metric 'aethertunnel_connections_rate_limited_total' -Port $guardDashboardPort) -ne 0) {
        throw "a connection inside the burst was rate limited"
    }
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        Attempt-ControlConnection -Port $guardControlPort | Out-Null
    }
    $deadline = (Get-Date).AddSeconds(10)
    while ($true) {
        $limited = Get-Metric 'aethertunnel_connections_rate_limited_total' -Port $guardDashboardPort
        if ($limited -ge 3) { break }
        if ((Get-Date) -gt $deadline) { throw "three attempts past the burst produced $limited rate-limit refusals" }
        Start-Sleep -Milliseconds 200
    }
    $audit = Get-Content -Raw $guardAudit
    if ($audit -notmatch '"event":"rate_limited"') { throw "the rate limit refusal was not audited" }
    if ((Read-Log $guardLog) -notmatch 'denied by rate limit') { throw "the server did not log the refusal" }
    return $true
}

Test-Check 'the audit log names the source that was refused' {
    $events = @(Read-AuditLines $guardAudit | Where-Object { $_ -match '"event":"acl_denied"' })
    if ($events.Count -lt 1) { throw "no acl_denied record" }
    if ($events[0] -notmatch '127\.0\.0\.2') { throw "the record does not name the source: $($events[0])" }
    return $true
}

if ($guard -and -not $guard.HasExited) { Stop-Process -Id $guard.Id -Force -ErrorAction SilentlyContinue }

Write-Step "checking the rotation of the audit log"

# A server of its own with a small audit.max_bytes, so the log rotates while the
# checks run instead of staying one file that grows without bound. Every attempt from
# a denied source writes exactly one record, which is the cheapest way to produce
# enough of them, and denying the loopback address needs no second source address.
$rotateControlPort = Get-FreePort
$rotateAudit = Join-Path $Root 'rotate-audit.jsonl'
$rotateToml = Join-Path $Root 'rotate.toml'
@"
[server]
bind_addr = "127.0.0.1"
bind_port = $rotateControlPort
auth_token = "$token"
deny_cidrs = ["127.0.0.1/32"]

[audit]
enabled = true
path = "$RootFwd/rotate-audit.jsonl"
max_bytes = 1024
keep = 2
"@ | Set-Content -Path $rotateToml -Encoding UTF8

$rotateLog = Join-Path $Root 'rotate.log'
$rotate = Start-Background -FilePath $serverExe -Arguments @('-config', $rotateToml) -LogPath $rotateLog -WorkingDirectory $Root
Wait-ForPort -Port $rotateControlPort | Out-Null
Start-Sleep -Seconds 1

Test-Check 'the audit log rotates once it reaches audit.max_bytes' {
    # One refused connection is one record of a few hundred bytes, so thirty attempts
    # cross a one kilobyte limit several times over.
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        Attempt-ControlConnection -Port $rotateControlPort | Out-Null
    }

    $rotated = "$rotateAudit.1"
    $deadline = (Get-Date).AddSeconds(10)
    while (-not (Test-Path $rotated) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 200 }
    if (-not (Test-Path $rotated)) {
        $size = 0
        try { $size = Get-FileSize $rotateAudit } catch { $size = -1 }
        throw "thirty refused connections left the audit log at $size bytes with no previous generation beside it, although max_bytes is 1024"
    }

    $rotatedSize = Get-FileSize $rotated
    if ($rotatedSize -le 0) { throw "the rotated generation is empty" }
    # The size is checked as a record is about to be written, so a generation reaches
    # the limit and then holds at most one record more than it.
    if ($rotatedSize -gt 2048) { throw "the rotated generation is $rotatedSize bytes, more than one record past the 1024 limit" }

    # A generation that ends mid-record is not something an operator can read, so
    # every line has to parse.
    $parsed = 0
    $malformed = 0
    foreach ($line in (Read-AuditLines $rotated)) {
        try { $null = $line | ConvertFrom-Json; $parsed++ } catch { $malformed++ }
    }
    if ($malformed -ne 0) { throw "$malformed of the $($parsed + $malformed) lines in the rotated generation do not parse" }
    if ($parsed -lt 1) { throw "the rotated generation holds no record" }

    $live = Get-FileSize $rotateAudit
    if ($live -ge $rotatedSize) {
        throw "the live log is $live bytes and the rotated one ${rotatedSize}: the log did not restart from a smaller size"
    }
    return $true
}

Test-Check 'the rotated generation holds the records written before it' {
    $rotated = "$rotateAudit.1"
    if (-not (Test-Path $rotated)) { throw "the audit log never rotated, so there is no previous generation to read" }
    $records = @(Read-AuditLines $rotated | Where-Object { $_ -match '"event":"acl_denied"' })
    if ($records.Count -lt 1) { throw "the rotated generation holds no acl_denied record" }
    if ($records[0] -notmatch '127\.0\.0\.1') { throw "the rotated record does not name the source: $($records[0])" }
    return $true
}

Test-Check 'audit.keep decides how many generations survive a rotation' {
    # This server keeps two, so a second rotation has to leave two generations
    # beside the live file and drop the one that is now older than both. Before
    # this check existed the setting was applied but nothing ever observed the
    # shift, and a bug that overwrote .1 every time would have looked the same.
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        Attempt-ControlConnection -Port $rotateControlPort | Out-Null
    }
    $second = "$rotateAudit.2"
    $deadline = (Get-Date).AddSeconds(10)
    while (((-not (Test-Path $second)) -or (-not (Test-Path "$rotateAudit.1"))) -and (Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 200
    }
    if (-not (Test-Path $second)) {
        $live = -1
        try { $live = Get-FileSize $rotateAudit } catch { }
        throw "thirty more refused connections left the live log at $live bytes with no second generation, although audit.keep is 2"
    }
    if (Test-Path "$rotateAudit.3") {
        throw "a third generation exists although audit.keep is 2"
    }

    # Both generations hold whole records, and .2 holds the ones written earlier.
    $newest = $null
    $older = $null
    foreach ($pair in @(@("$rotateAudit.2", 'older'), @("$rotateAudit.1", 'newest'))) {
        $lines = Read-AuditLines $pair[0]
        if ($lines.Count -lt 1) { throw "$($pair[0]) holds no record" }
        foreach ($line in $lines) {
            try { $null = $line | ConvertFrom-Json } catch {
                throw "$($pair[0]) holds a line that does not parse: $line"
            }
        }
        $first = $lines[0] | ConvertFrom-Json
        if ($pair[1] -eq 'older') { $older = $first.Time } else { $newest = $first.Time }
    }
    if ([datetime]$newest -lt [datetime]$older) {
        throw "the newest kept generation starts at $newest, before the older one at $older"
    }
    return $true
}

if ($rotate -and -not $rotate.HasExited) { Stop-Process -Id $rotate.Id -Force -ErrorAction SilentlyContinue }

Write-Step "checking that an audit log which cannot be written is reported"

# A server of its own. Denying the loopback address produces one audit record per
# attempt without needing a client, so the log is easy to drive.
#
# The failure is made the way it happens in practice: write access to the log and to its
# directory is taken away while the server runs. The server keeps working through the
# handle it already holds, so nothing fails until it has to open the path again, which it
# does before every record and on every rotation. Without that check it would keep
# "writing" into a file nobody can read while everything else looked healthy.
#
# On Windows the denial has to cover AppendData as well as WriteData: an open for append
# asks for the append right, so denying only WriteData leaves it working.
$auditHealthDir = Join-Path $Root 'audit-health'
New-Item -ItemType Directory -Force -Path $auditHealthDir | Out-Null
$auditHealthPath = Join-Path $auditHealthDir 'audit.jsonl'
$auditHealthOwner = "$env:USERDOMAIN\$env:USERNAME"
$auditHealthControlPort = Get-FreePort
$auditHealthDashboardPort = Get-FreePort
$auditHealthToml = Join-Path $Root 'audit-health.toml'
@"
[server]
bind_addr = "127.0.0.1"
bind_port = $auditHealthControlPort
auth_token = "$token"
deny_cidrs = ["127.0.0.1/32"]

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $auditHealthDashboardPort

[metrics]
enabled = true

[audit]
enabled = true
path = "$RootFwd/audit-health/audit.jsonl"
# Small enough that a handful of records crosses it, which is what makes the server
# reopen the path and therefore notice that it can no longer write.
max_bytes = 1024
"@ | Set-Content -Path $auditHealthToml -Encoding UTF8

$auditHealthLog = Join-Path $Root 'audit-health.log'
$auditHealth = Start-Background -FilePath $serverExe -Arguments @('-config', $auditHealthToml) -LogPath $auditHealthLog -WorkingDirectory $Root
Wait-ForPort -Port $auditHealthControlPort | Out-Null
Start-Sleep -Seconds 1

Test-Check 'a healthy audit log is reported as writable and complete' {
    for ($attempt = 0; $attempt -lt 3; $attempt++) {
        Attempt-ControlConnection -Port $auditHealthControlPort | Out-Null
    }
    $deadline = (Get-Date).AddSeconds(10)
    while (-not (Test-Path $auditHealthPath) -or (Get-FileSize $auditHealthPath) -eq 0) {
        if ((Get-Date) -gt $deadline) { throw "the audit log was never written" }
        Start-Sleep -Milliseconds 200
    }

    $status = Get-Status -Port $auditHealthDashboardPort
    if ($status.audit.enabled -ne $true) { throw "the status endpoint reports the audit log as $($status.audit.enabled)" }
    if ($status.audit.writable -ne $true) { throw "a writable audit log is reported as not writable" }
    if ($status.audit.records_lost -ne 0) { throw "a healthy audit log reports $($status.audit.records_lost) lost records" }
    if ($status.audit.last_error) { throw "a healthy audit log reports the error '$($status.audit.last_error)'" }
    # The configured path is reported as it was written, with forward slashes, so the
    # comparison normalises the separator it is compared against.
    if (($status.audit.path -replace '\\', '/') -ne ($auditHealthPath -replace '\\', '/')) {
        throw "the summary names '$($status.audit.path)'"
    }
    if ((Get-Metric 'aethertunnel_audit_records_lost_total' -Port $auditHealthDashboardPort) -ne 0) {
        throw "the lost-records counter is not zero on a healthy server"
    }
    return $true
}

Test-Check 'a server whose audit log cannot be written says so and keeps serving' {
    # Take write and append access away from the log and from its directory. The handle
    # the server already holds keeps working, so the failure appears when it next has to
    # open the path.
    try {
        icacls $auditHealthPath /deny "${auditHealthOwner}:(WD,AD)" | Out-Null
        icacls $auditHealthDir /deny "${auditHealthOwner}:(WD,AD)" | Out-Null

        # Enough records to cross max_bytes, which is what forces the rotation that
        # reopens the path.
        for ($attempt = 0; $attempt -lt 20; $attempt++) {
            Attempt-ControlConnection -Port $auditHealthControlPort | Out-Null
        }

        $deadline = (Get-Date).AddSeconds(20)
        $lost = 0
        while ($true) {
            $lost = (Get-Metric 'aethertunnel_audit_records_lost_total' -Port $auditHealthDashboardPort)
            if ($lost -ge 1) { break }
            if ((Get-Date) -gt $deadline) {
                throw "20 attempts after the log became unwritable left the lost-records counter at $lost"
            }
            Start-Sleep -Milliseconds 200
        }

        $status = Get-Status -Port $auditHealthDashboardPort
        if ($status.audit.enabled -ne $true) {
            throw "a broken audit log reports itself disabled instead of enabled-but-broken"
        }
        if ($status.audit.writable -ne $false) { throw "a broken audit log still reports itself as writable" }
        if ($status.audit.records_lost -lt 1) { throw "the summary reports $($status.audit.records_lost) lost records" }
        if (-not $status.audit.last_error) { throw "the summary names no error although every write fails" }
        if ((Get-Metric 'aethertunnel_audit_write_failures_total' -Port $auditHealthDashboardPort) -lt 1) {
            throw "the write-failure counter never moved"
        }

        # The server itself is unaffected: this is not a fatal condition and must not be.
        $health = Invoke-Curl @('-s', '-o', 'NUL', '-w', '%{http_code}', "http://127.0.0.1:$auditHealthDashboardPort/healthz")
        if ($health -ne '200') { throw "/healthz answered $health while the audit log was broken" }
        $status = Get-Status -Port $auditHealthDashboardPort
        if ($status.connections.total -lt 1) { throw "the server stopped counting connections" }
        if ((Read-Log $auditHealthLog) -notmatch 'audit: cannot write') {
            throw "the server did not log that it cannot write its audit log"
        }
    } finally {
        # Always put the permissions back, or the working directory cannot be removed.
        icacls $auditHealthDir /remove:d $auditHealthOwner 2>&1 | Out-Null
        icacls $auditHealthPath /remove:d $auditHealthOwner 2>&1 | Out-Null
    }
    return $true
}

Test-Check 'the audit log starts recording again once its path is writable' {
    # The permissions were restored by the check above. The auditor opens the path again
    # on the next record, so nothing has to be restarted and no further record is lost.
    for ($attempt = 0; $attempt -lt 3; $attempt++) {
        Attempt-ControlConnection -Port $auditHealthControlPort | Out-Null
    }

    $deadline = (Get-Date).AddSeconds(20)
    while ($true) {
        $status = Get-Status -Port $auditHealthDashboardPort
        if ($status.audit.writable -eq $true -and -not $status.audit.last_error) { break }
        if ((Get-Date) -gt $deadline) {
            throw "the audit log is still reported as not writable ('$($status.audit.last_error)') 20s after its path was writable"
        }
        Start-Sleep -Milliseconds 200
    }

    if (-not (Test-Path $auditHealthPath)) { throw "the audit log was not recreated at its configured path" }
    $records = @(Read-AuditLines $auditHealthPath | Where-Object { $_ -match '"event":"acl_denied"' })
    if ($records.Count -lt 1) { throw "the recreated audit log holds no record" }
    if ((Read-Log $auditHealthLog) -notmatch 'is writable again') {
        throw "the server did not log that its audit log recovered"
    }
    return $true
}

if ($auditHealth -and -not $auditHealth.HasExited) { Stop-Process -Id $auditHealth.Id -Force -ErrorAction SilentlyContinue }

Test-Check 'a server whose audit log cannot be opened refuses to start' {
    # The other half of the audit story: when the very first open fails there is nothing
    # to report at runtime, so the server has to refuse to start and say why rather than
    # come up with no audit trail at all.
    $blockedDir = Join-Path $Root 'audit-blocked'
    New-Item -ItemType Directory -Force -Path (Join-Path $blockedDir 'audit.jsonl') | Out-Null
    $blockedToml = Join-Path $Root 'audit-blocked.toml'
    @"
[server]
bind_addr = "127.0.0.1"
bind_port = $(Get-FreePort)
auth_token = "$token"

[audit]
enabled = true
path = "$RootFwd/audit-blocked/audit.jsonl"
"@ | Set-Content -Path $blockedToml -Encoding UTF8

    $output = Invoke-Binary -FilePath $serverExe -Arguments @('-config', $blockedToml) -AllowFailure
    if ($output -notmatch 'open audit log') {
        throw "the server did not report that it cannot open its audit log: $output"
    }
    if ($output -notmatch 'aethertunnel-audit' -and $output -notmatch 'audit\.jsonl') {
        throw "the message does not name the path it could not open: $output"
    }
    return $true
}

Write-Step "summary"
Write-Host ""
Write-Host ("   checks passed: " + $script:Passes) -ForegroundColor Green
if ($script:Failures.Count -gt 0) {
    Write-Host ("   checks failed: " + $script:Failures.Count) -ForegroundColor Red
    foreach ($failure in $script:Failures) { Write-Host ("     - " + $failure) -ForegroundColor Red }
}

Write-Host ""
Write-Host "   logs and configuration: $Root"

if (-not $Keep) {
    foreach ($process in $script:Processes) {
        if ($process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        }
    }
    Start-Sleep -Milliseconds 500
    Remove-Item -Recurse -Force $Root -ErrorAction SilentlyContinue
    Write-Host "   stopped every process and removed the working directory"
}

if ($script:Failures.Count -gt 0) { exit 1 }
exit 0
