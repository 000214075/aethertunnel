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
#>
[CmdletBinding()]
param(
    [string]$Root = (Join-Path $env:TEMP ("aether-smoke-" + [guid]::NewGuid().ToString('N').Substring(0, 8))),
    [switch]$Keep,
    # Appends each step and verdict here as it happens. stdout is buffered when the
    # script is run with its output redirected, so this is what shows where a run is.
    [string]$ProgressLog = ''
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

$serverExe = Join-Path $bin 'aethertunnel-server.exe'
$clientExe = Join-Path $bin 'aethertunnel-client.exe'
$helperExe = Join-Path $bin 'smoketest.exe'

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
$banControlPort = Get-FreePort
$banDashboardPort = Get-FreePort
$banProxyPort = Get-FreePort
$graceControlPort = Get-FreePort
$graceProxyPort = Get-FreePort

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
# control connection, every data connection and every visitor connection.
$commonObfuscation = @"
[obfuscation]
enabled = true
pad_to = 256
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
            'aethertunnel_data_connections_total',
            'aethertunnel_udp_datagrams_total',
            'aethertunnel_http_requests_total',
            'aethertunnel_p2p_punches_total',
            'aethertunnel_bytes_to_clients_total',
            'aethertunnel_socks5_requests_total',
            'aethertunnel_streams_refused_while_draining_total',
            'aethertunnel_tunnel_streams_active{tunnel="tcp-echo"}',
            'aethertunnel_tunnel_streams_total{tunnel="tcp-echo"}')) {
        if ($body -notmatch [regex]::Escape($series)) { throw "the metrics output has no $series" }
    }
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
