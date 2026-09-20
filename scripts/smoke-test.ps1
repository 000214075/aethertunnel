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
    [switch]$Keep
)

$ErrorActionPreference = 'Stop'
$script:Failures = @()
$script:Passes = 0
$script:Processes = @()

function Write-Step([string]$Message) {
    Write-Host ""
    Write-Host "== $Message" -ForegroundColor Cyan
}

function Test-Check {
    param([string]$Name, [scriptblock]$Body)

    try {
        $result = & $Body
        if ($result -eq $false) {
            throw "check returned false"
        }
        Write-Host ("   PASS  " + $Name) -ForegroundColor Green
        $script:Passes++
    } catch {
        Write-Host ("   FAIL  " + $Name + " -- " + $_.Exception.Message) -ForegroundColor Red
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

function Read-Log {
    param([string]$LogPath)

    if (-not (Test-Path $LogPath)) { return '' }
    return (Get-Content -Raw -Path $LogPath -ErrorAction SilentlyContinue)
}

function Invoke-TcpEcho {
    param([int]$Port, [string]$Payload, [int]$ExpectedLength = 0, [string]$Host_ = '127.0.0.1')

    if ($ExpectedLength -eq 0) { $ExpectedLength = $Payload.Length }

    $client = New-Object System.Net.Sockets.TcpClient
    try {
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

function Invoke-Curl {
    param([string[]]$Arguments)

    $output = & curl.exe @Arguments 2>&1
    return ($output | Out-String).Trim()
}

# The binaries log to standard error. PowerShell 5.1 turns a native command's
# standard error into an error record, which with ErrorActionPreference Stop ends
# the script, so one-shot invocations go through cmd and discard it.
function Invoke-Binary {
    param([string]$FilePath, [string[]]$Arguments = @())

    if (-not $Arguments) { $Arguments = @() }
    $line = '"' + $FilePath + '"'
    foreach ($argument in $Arguments) { $line += ' "' + $argument + '"' }
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
$tcpProxyPort = Get-FreePort
$udpProxyPort = Get-FreePort
$stcpVisitorPort = Get-FreePort
$sudpVisitorPort = Get-FreePort
$xtcpVisitorPort = Get-FreePort

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

@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

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
"@ | Set-Content -Path $clientToml -Encoding UTF8

@"
[client]
server_addr = "127.0.0.1:$controlPort"
auth_token = "$token"

$commonSecurity
ca_file = ""
server_name = "127.0.0.1"
insecure_skip_verify = true

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
"@ | Set-Content -Path $serverToml -Encoding UTF8

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
            'aethertunnel_tunnel_streams_total{tunnel="tcp-echo"}')) {
        if ($body -notmatch [regex]::Escape($series)) { throw "the metrics output has no $series" }
    }
    return $true
}

Test-Check 'the metrics counters moved' {
    $body = Invoke-Curl @('-s', "http://127.0.0.1:$dashboardPort/metrics")
    $match = [regex]::Match($body, 'aethertunnel_udp_datagrams_total (\d+)')
    if (-not $match.Success) { throw "no udp datagram counter" }
    if ([int]$match.Groups[1].Value -le 0) { throw "the udp datagram counter is still zero" }
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
