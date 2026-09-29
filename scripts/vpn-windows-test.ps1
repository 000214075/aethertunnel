# Runs the layer-3 tunnel ([vpn]) on real Wintun adapters on this machine, and
# proves what one machine can prove: both ends open their adapters, the addresses
# the server hands out land on them, the state reaches /api/vpn, and a client that
# must be on the tunnel is refused without it. A packet between two adapters on ONE
# machine cannot be proven to have entered the driver - the local routing table
# shortcuts it - so the end-to-end packet path is proven on Linux by
# scripts/vpn-linux-test.sh, which shares every forwarding line with this build.
#
# Requirements: administrator (creating a Wintun adapter needs it) and the official
# wintun.dll, downloaded from wintun.net when missing. The program loads it through
# WINTUN_DLL and never installs a driver.
#
# Usage: scripts/vpn-windows-test.ps1 -Server <server.exe> -Client <client.exe>

param(
    [Parameter(Mandatory = $true)][string]$Server,
    [Parameter(Mandatory = $true)][string]$Client
)

$ErrorActionPreference = "Stop"
$script:failures = 0
function Pass($m) { Write-Output "PASS  $m" }
function Fail($m, $d) { if ($d) { Write-Output "FAIL  $m -> $d" } else { Write-Output "FAIL  $m" }; $script:failures++ }

$identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "creating a Wintun adapter needs administrator rights; run this script elevated"
}
if (-not (Test-Path $Server)) { throw "$Server is missing" }
if (-not (Test-Path $Client)) { throw "$Client is missing" }

$work = Join-Path ([IO.Path]::GetTempPath()) ("aether-vpn-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Path $work | Out-Null
$control = 27182
$dashboard = 27583
$net = "10.63.7"
$serverDevice = "AetherTunnelServer"
$clientDevice = "AetherTunnelClient"

$dllDir = Join-Path $env:RUNNER_TEMP "wintun"
$dll = Join-Path $dllDir "wintun/bin/amd64/wintun.dll"
if (-not (Test-Path $dll)) {
    New-Item -ItemType Directory -Path $dllDir -Force | Out-Null
    Invoke-WebRequest -Uri "https://www.wintun.net/builds/wintun-0.14.1.zip" -OutFile "$dllDir\wintun.zip"
    Expand-Archive -Path "$dllDir\wintun.zip" -DestinationPath $dllDir -Force
}
$env:WINTUN_DLL = $dll

@"
[server]
bind_addr = "127.0.0.1"
bind_port = $control
auth_token = "vpn-windows-test-token-0123456789"

[dashboard]
enabled = true
bind_addr = "127.0.0.1"
port = $dashboard
token = "vpn-windows-test-dashboard"

[vpn]
enabled = true
device = "$serverDevice"
address = "$net.0/24"
require = true
"@ | Set-Content "$work\server.toml"

@"
[client]
server_addr = "127.0.0.1:$control"
auth_token = "vpn-windows-test-token-0123456789"

[vpn]
enabled = true
device = "$clientDevice"
"@ | Set-Content "$work\client.toml"

@"
[client]
server_addr = "127.0.0.1:$control"
auth_token = "vpn-windows-test-token-0123456789"
"@ | Set-Content "$work\novpn.toml"

$serverProc = Start-Process -FilePath $Server -ArgumentList "--config", "$work\server.toml" -RedirectStandardOutput "$work\server.log" -RedirectStandardError "$work\server.err" -PassThru -WindowStyle Hidden
$deadline = (Get-Date).AddSeconds(30)
do {
    Start-Sleep -Milliseconds 500
    $listening = Test-NetConnection -ComputerName 127.0.0.1 -Port $control -InformationLevel Quiet -WarningAction SilentlyContinue
} while (((Get-Date) -lt $deadline) -and (-not $listening))
if ($listening) { Pass "the server is listening on its control port" } else { Fail "the server never listened" ((Get-Content "$work\server.log" -Tail 3 -ErrorAction SilentlyContinue) + (Get-Content "$work\server.err" -Tail 3 -ErrorAction SilentlyContinue) -join " | ") }

if (Select-String -Path "$work\server.log", "$work\server.err" -Pattern "vpn: interface $serverDevice" -Quiet) {
    Pass "the server opened a real Wintun adapter"
} else { Fail "the server did not open the adapter" ((Get-Content "$work\server.log" -Tail 3 -ErrorAction SilentlyContinue) + (Get-Content "$work\server.err" -Tail 3 -ErrorAction SilentlyContinue) -join " | ") }

$clientProc = Start-Process -FilePath $Client -ArgumentList "--config", "$work\client.toml" -RedirectStandardOutput "$work\client.log" -RedirectStandardError "$work\client.err" -PassThru -WindowStyle Hidden

$deadline = (Get-Date).AddSeconds(30)
do {
    Start-Sleep -Milliseconds 500
    # The client logs to stderr, so the announcement can be in either file.
    $clientUp = Select-String -Path "$work\client.log", "$work\client.err" -Pattern "tunnel interface $clientDevice" -Quiet
} while (((Get-Date) -lt $deadline) -and (-not $clientUp))
if ($clientUp) { Pass "the client opened its adapter and took the address the server handed out" } else { Fail "the client never configured a tunnel interface" ((Get-Content "$work\client.log" -Tail 3 -ErrorAction SilentlyContinue) + (Get-Content "$work\client.err" -Tail 3 -ErrorAction SilentlyContinue) -join " | ") }

# Both addresses are on the adapters the kernel owns, visible to the system.
$serverIP = (Get-NetIPAddress -InterfaceAlias $serverDevice -AddressFamily IPv4 -ErrorAction SilentlyContinue).IPAddress
if ($serverIP -eq "$net.1") { Pass "the server's adapter has $net.1" } else { Fail "the server's adapter address is '$serverIP', want $net.1" }
$clientIP = (Get-NetIPAddress -InterfaceAlias $clientDevice -AddressFamily IPv4 -ErrorAction SilentlyContinue).IPAddress
if ($clientIP -eq "$net.2") { Pass "the client's adapter has $net.2" } else { Fail "the client's adapter address is '$clientIP', want $net.2" }

# The same state, seen the way an operator sees it: through the dashboard API,
# which reports the server's own summary - how many addresses the pool handed
# out, and how many peers hold one.
Start-Sleep -Seconds 1
$api = Invoke-RestMethod -Headers @{ Authorization = "Bearer vpn-windows-test-dashboard" } "http://127.0.0.1:$dashboard/api/vpn"
if ($api.addresses_used -ge 1 -and $api.peers -ge 1 -and $api.server_address -eq "$net.1") {
    Pass "the dashboard's /api/vpn reports the handed-out address and the peer holding it"
} else { Fail "/api/vpn does not report the handed-out address and the peer" (($api | ConvertTo-Json -Depth 6).Substring(0, 400)) }

# A session that must be on the tunnel is refused without it: the no-vpn client's
# registration fails, and the refusal is visible in its own log.
$novpnProc = Start-Process -FilePath $Client -ArgumentList "--config", "$work\novpn.toml" -RedirectStandardOutput "$work\novpn.log" -RedirectStandardError "$work\novpn.err" -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 3
if (Select-String -Path "$work\novpn.log", "$work\server.log", "$work\server.err" -Pattern "refus|require" -Quiet) {
    Pass "a client that is not on the tunnel is refused (vpn.require)"
} else { Fail "a client that is not on the tunnel was not refused" ((Get-Content "$work\novpn.log" -Tail 3 -ErrorAction SilentlyContinue) + (Get-Content "$work\server.err" -Tail 3 -ErrorAction SilentlyContinue) -join " | ") }

# Informational, not a check: two adapters on one machine cannot demonstrate that
# a packet entered the driver. See the file comment.
$ping = Start-Process ping -ArgumentList "-n", "1", "-S", "$clientIP", "$net.1" -Wait -PassThru -NoNewWindow -RedirectStandardOutput "$work\ping.txt"
Get-Content "$work\ping.txt" | Select-String "Reply|timed out|unreachable|一般故障|无法访问" | ForEach-Object { Write-Output "INFO  ping -S ${clientIP} ${net}.1: $_" }

Stop-Process -Id $clientProc.Id -Force -ErrorAction SilentlyContinue
Stop-Process -Id $novpnProc.Id -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1
Stop-Process -Id $serverProc.Id -Force -ErrorAction SilentlyContinue

if ($script:failures -eq 0) {
    Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
    Write-Output "ALL WINDOWS VPN CHECKS PASSED"
    exit 0
}
Write-Output "checks failed: $script:failures   logs kept in $work"
exit 1
