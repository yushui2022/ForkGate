param([string] $BaseImage = 'python:3.13.15-slim-bookworm')
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

# Docker creates the snapshot and children. ForkGate only registers lineage.
# This experiment uses an explicit HTTP proxy, not memory checkpoint/restore.
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$runID = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$prefix = "forkgate-$runID"
$runDir = Join-Path $projectRoot "output\docker-sandbox\$runID"
$buildDir = Join-Path $projectRoot 'output\docker-sandbox\build'
$go = 'G:\DevCache\go\go1.26.8\bin\go.exe'
$gatewayImage = "forkgate-demo-gateway:$runID"
$snapshotImage = "forkgate-demo-snapshot:$runID"
$network = "$prefix-net"
$gateway = "$prefix-gateway"
$upstream = "$prefix-upstream"
$parent = "$prefix-parent"
$children = @(1..3 | ForEach-Object { "$prefix-child-$_" })
$containers = [System.Collections.Generic.List[string]]::new()
$images = [System.Collections.Generic.List[string]]::new()
$networks = [System.Collections.Generic.List[string]]::new()
$result = [ordered]@{ run_id = $runID; status = 'running'; backend = 'Docker Desktop Linux containers'; fork_mode = 'docker commit filesystem snapshot; no memory/process restore'; network_mode = 'explicit HTTP proxy on an isolated user-defined Docker bridge; localhost-only published ports' }

function Invoke-Docker([string[]] $Arguments) {
    $lines = @(& docker @Arguments)
    if ($LASTEXITCODE -ne 0) { throw "Docker operation '$($Arguments[0])' failed (exit $LASTEXITCODE)." }
    return $lines
}
function Start-DemoContainer([string] $Name, [string[]] $Options) {
    $id = (Invoke-Docker (@('run', '-d', '--name', $Name, '--label', "forkgate.demo.run=$runID", '--memory', '256m', '--cpus', '1', '--pids-limit', '64') + $Options)) -join ''
    $containers.Add($id)
    return $id
}
function Get-Inspect([string] $Name) { return @(((Invoke-Docker @('inspect', $Name)) -join "`n") | ConvertFrom-Json)[0] }
function Get-FreePort([int] $Preferred) {
    for ($port = $Preferred; $port -lt ($Preferred + 100); $port++) {
        $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $port)
        try { $listener.Start(); return $port } catch {} finally { $listener.Stop() }
    }
    throw "No free TCP port near $Preferred"
}
function Wait-HTTP([string] $URL, [hashtable] $Headers = @{}) {
    for ($i = 0; $i -lt 30; $i++) { try { return Invoke-RestMethod -Uri $URL -Headers $Headers -TimeoutSec 2 -NoProxy } catch { if ($i -eq 29) { throw }; Start-Sleep -Milliseconds 500 } }
}
function Assert-Value($Actual, $Expected, [string] $Label) { if ($Actual -ne $Expected) { throw "$Label : expected $Expected; got $Actual" } }
function Invoke-Control([string] $Method, [string] $Path, $Body = $null) {
    $parameters = @{ Method = $Method; Uri = "$controlURL$Path"; Headers = $headers; TimeoutSec = 45; NoProxy = $true }
    if ($null -ne $Body) { $parameters.ContentType = 'application/json'; $parameters.Body = $Body | ConvertTo-Json -Compress }
    return Invoke-RestMethod @parameters
}
function Invoke-Workload([int] $ChildIndex, [string] $Token, [string] $Method, [string] $Path, [string] $Body = '') {
    $dockerArgs = @('exec', '-e', "FORKGATE_PROXY=http://${gateway}:3128", '-e', "FORKGATE_TOKEN=$Token", '-e', "FORKGATE_DEMO_BRANCH=child-$($ChildIndex + 1)", $children[$ChildIndex], 'python', '/workload.py', $Method, "http://${upstream}:8080$Path")
    if ($Body -ne '') { $dockerArgs += $Body }
    return ((Invoke-Docker $dockerArgs) -join "`n") | ConvertFrom-Json
}

New-Item -ItemType Directory -Force -Path $runDir, $buildDir | Out-Null
try {
    $server = ((Invoke-Docker @('version', '--format', '{{json .Server}}')) -join '') | ConvertFrom-Json
    Assert-Value $server.Os 'linux' 'Docker engine'
    $result.docker_version = $server.Version
    $base = ((Invoke-Docker @('image', 'inspect', $BaseImage)) -join "`n") | ConvertFrom-Json
    $result.base_image_id = @($base)[0].Id
    $architecture = @($base)[0].Architecture
    if ($architecture -ne 'amd64') { throw "This local smoke image currently requires amd64; got $architecture" }
    Write-Host "Docker ready. Building gateway for linux/$architecture; output: $runDir"
    $buildEnv = @{ GOOS = 'linux'; GOARCH = $architecture; CGO_ENABLED = '0'; GOPATH = "$env:GOPATH"; GOMODCACHE = 'G:\DevCache\go-mod'; GOCACHE = 'G:\DevCache\go-build'; GOTMPDIR = 'G:\DevCache\Temp\ForkGate\go'; TEMP = 'G:\DevCache\Temp\ForkGate\go'; TMP = 'G:\DevCache\Temp\ForkGate\go' }
    $savedEnv = @{}
    New-Item -ItemType Directory -Force -Path $buildEnv.GOTMPDIR | Out-Null
    try {
        foreach ($key in $buildEnv.Keys) { $savedEnv[$key] = [Environment]::GetEnvironmentVariable($key, 'Process'); [Environment]::SetEnvironmentVariable($key, $buildEnv[$key], 'Process') }
        Push-Location $projectRoot
        try { & $go build -mod=readonly -trimpath -o (Join-Path $buildDir 'forkgate-linux-amd64') ./cmd/forkgate; if ($LASTEXITCODE -ne 0) { throw 'Linux gateway build failed' } } finally { Pop-Location }
    } finally { foreach ($key in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($key, $savedEnv[$key], 'Process') } }
    Invoke-Docker @('build', '--tag', $gatewayImage, '--file', (Join-Path $PSScriptRoot 'Dockerfile.gateway'), $buildDir) | Out-Null; $images.Add($gatewayImage)
    # A private user-defined bridge keeps the experiment separate while still
    # allowing host-local port probes; --internal would suppress published
    # ports on Docker Desktop and make the control plane unreachable.
    $networkID = (Invoke-Docker @('network', 'create', '--label', "forkgate.demo.run=$runID", $network)) -join ''; $networks.Add($networkID)
    $adminToken = [guid]::NewGuid().ToString('N'); $masterKey = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N')
    $proxyPort = Get-FreePort 3130; $controlPort = Get-FreePort ($proxyPort + 1); $upstreamPort = Get-FreePort 18080
    Start-DemoContainer $gateway @('--network', $network, '-p', "127.0.0.1:${proxyPort}:3128", '-p', "127.0.0.1:${controlPort}:7070", '-e', 'FORKGATE_BRANCHES_ENABLED=1', '-e', "FORKGATE_MASTER_KEY=$masterKey", '-e', 'FORKGATE_ALLOW_NON_LOOPBACK=1', $gatewayImage, '-admin-token', $adminToken, '-data-dir', '/data', '-proxy-addr', '0.0.0.0:3128', '-control-addr', '0.0.0.0:7070') | Out-Null
    Start-DemoContainer $upstream @('--network', $network, '-p', "127.0.0.1:${upstreamPort}:8080", '--mount', "type=bind,source=$(Join-Path $PSScriptRoot 'upstream.py'),target=/upstream.py,readonly", $BaseImage, 'python', '/upstream.py') | Out-Null
    $controlURL = "http://127.0.0.1:$controlPort"; $upstreamURL = "http://127.0.0.1:$upstreamPort"; $headers = @{ Authorization = "Bearer $adminToken" }
    Wait-HTTP "$controlURL/healthz" $headers | Out-Null; Wait-HTTP "$upstreamURL/summary" | Out-Null

    Start-DemoContainer $parent @('--network', 'none', $BaseImage, 'sleep', '600') | Out-Null
    Invoke-Docker @('cp', (Join-Path $PSScriptRoot 'workload.py'), "${parent}:/workload.py") | Out-Null
    $prepareCode = 'import pathlib,sys; p=pathlib.Path("/checkpoint"); p.mkdir(exist_ok=True); p.chmod(0o777); (p/"seed").write_text(sys.argv[1])'
    Invoke-Docker @('exec', $parent, 'python', '-c', $prepareCode, $runID) | Out-Null
    $snapshotID = (Invoke-Docker @('commit', '--message', 'ForkGate local filesystem lineage experiment', $parent, $snapshotImage)) -join ''; $images.Add($snapshotImage); $result.snapshot_image_id = $snapshotID
    Invoke-Docker @('stop', $parent) | Out-Null

    $tree = Invoke-Control 'Post' '/v1/trees'; $lineage = @()
    for ($i = 0; $i -lt $children.Count; $i++) {
        $childID = Start-DemoContainer $children[$i] @('--network', $network, '--user', '65534:65534', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true', $snapshotImage, 'sleep', '600')
        $info = Get-Inspect $children[$i]; Assert-Value $info.Image $snapshotID 'child snapshot image'
        $seed = (Invoke-Docker @('exec', $children[$i], 'cat', '/checkpoint/seed')) -join ''; Assert-Value $seed $runID 'inherited checkpoint seed'
        $lineage += [ordered]@{ container_id = $childID; image_id = $info.Image; inherited_seed = $seed; branch_id = $null }
    }
    Invoke-Docker @('exec', $children[0], 'python', '-c', 'from pathlib import Path; Path("/checkpoint/child1-only").write_text("private")') | Out-Null
    foreach ($other in $children[1..2]) { $exists = (Invoke-Docker @('exec', $other, 'python', '-c', 'from pathlib import Path; print(Path("/checkpoint/child1-only").exists())')) -join ''; Assert-Value $exists 'False' 'child writable layer isolation' }
    $fork = Invoke-Control 'Post' "/v1/branches/$($tree.root_branch.branch_id)/fork" @{ count = $children.Count }
    for ($i = 0; $i -lt $children.Count; $i++) { $lineage[$i].branch_id = $fork.children[$i].branch_id }
    $result.lineage = [ordered]@{ parent_container_id = $parent; snapshot_image_id = $snapshotID; tree_id = $tree.tree_id; children = $lineage; independent_writable_layers = $true }

    $stale = Invoke-Workload 0 $tree.token 'POST' '/effects' '{"operation":"old-parent"}'; Assert-Value $stale.status 503 'sealed parent identity'
    $calls = @()
    for ($i = 0; $i -lt $children.Count; $i++) {
        $read = Invoke-Workload $i $fork.children[$i].token 'GET' '/read'; Assert-Value $read.status 200 'read through proxy'
        foreach ($seq in 1..2) { $body = @{ operation = 'create-issue'; seq = $seq } | ConvertTo-Json -Compress; $write = Invoke-Workload $i $fork.children[$i].token 'POST' '/effects' $body; Assert-Value $write.status 202 'speculative write'; if (-not $write.staged_id) { throw 'Missing staged ID' }; $calls += [ordered]@{ child = $i + 1; seq = $seq; status = $write.status; staged_id = $write.staged_id } }
    }
    $before = Wait-HTTP "$upstreamURL/summary"; Assert-Value $before.writes 0 'upstream writes before commit'; $result.before_commit = $before; $result.speculative_calls = $calls
    Invoke-Control 'Post' "/v1/branches/$($fork.children[0].branch_id)/abort" | Out-Null
    $afterAbort = Invoke-Workload 0 $fork.children[0].token 'GET' '/read'; Assert-Value $afterAbort.status 410 'aborted child token'
    $continued = Invoke-Workload 1 $fork.children[1].token 'GET' '/read'; Assert-Value $continued.status 200 'sibling still usable'
    $commit = Invoke-Control 'Post' "/v1/branches/$($fork.children[1].branch_id)/commit"; Assert-Value $commit.status 'committed' 'commit status'; Assert-Value $commit.items.Count 2 'committed write count'
    $states = @()
    for ($i = 0; $i -lt $children.Count; $i++) { $terminal = Invoke-Workload $i $fork.children[$i].token 'POST' '/effects' '{"operation":"after-terminal"}'; Assert-Value $terminal.status 410 'terminal child token'; $staged = Invoke-Control 'Get' "/v1/branches/$($fork.children[$i].branch_id)/staged"; Assert-Value $staged.staged.Count 2 'staged record count'; $expected = if ($i -eq 1) { 'succeeded' } else { 'discarded' }; foreach ($item in $staged.staged) { Assert-Value $item.status $expected 'staged write final state' }; $states += [ordered]@{ child = $i + 1; response_status = $terminal.status; writes = $staged.staged } }
    $after = Wait-HTTP "$upstreamURL/summary"; Assert-Value $after.writes 2 'upstream writes after commit'; Assert-Value $after.requests.Count 2 'upstream receipt count'
    for ($i = 0; $i -lt 2; $i++) { Assert-Value $after.requests[$i].branch 'child-2' 'upstream receipt branch'; Assert-Value $after.requests[$i].path '/effects' 'upstream receipt path'; $body = $after.requests[$i].body | ConvertFrom-Json; Assert-Value $body.operation 'create-issue' 'committed operation'; Assert-Value $body.seq ($i + 1) 'upstream receipt order' }
    $result.after_commit = $after; $result.branch_results = $states; $result.commit = $commit; $result.sealed_parent_status = $stale.status
    $direct = ((Invoke-Docker @('exec', $children[0], 'python', '/workload.py', '--direct', 'GET', "http://${upstream}:8080/direct-probe")) -join "`n") | ConvertFrom-Json; $result.direct_probe = @{ status = $direct.status; request = 'GET /direct-probe without proxy'; enforced_egress = $false }
    $result.status = 'passed'; Write-Host "PASS: only child-2 seq 1,2 reached upstream. Direct GET without proxy = $($direct.status) (bypass protection is not implemented)."
} catch { $result.status = 'failed'; $result.error = $_.Exception.Message; throw }
finally {
    $result.finished_at = (Get-Date).ToUniversalTime().ToString('o'); $result | ConvertTo-Json -Depth 15 | Set-Content -LiteralPath (Join-Path $runDir 'result.json') -Encoding utf8
    if ($containers.Count -gt 0) { & docker logs $gateway 2>&1 | Set-Content -LiteralPath (Join-Path $runDir 'gateway.log') -Encoding utf8 }
    foreach ($id in $containers) { & docker container rm --force $id 2>$null | Out-Null }
    foreach ($tag in $images) { & docker image rm $tag 2>$null | Out-Null }
    foreach ($id in $networks) { & docker network rm $id 2>$null | Out-Null }
    Write-Host "Report saved: $(Join-Path $runDir 'result.json')"
}
