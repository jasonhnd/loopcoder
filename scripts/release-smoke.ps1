#!/usr/bin/env pwsh
param(
    [string]$Version = "0.7.0",
    [string]$PreviousVersion = "0.6.1",
    [string]$Repo = "jasonhnd/loopcoder",
    [string]$GitHubBaseUrl = "https://github.com",
    [string]$GitHubApiUrl = "https://api.github.com"
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Fail([string]$Message) {
    Write-Error $Message
    exit 1
}

function Require-Command([string]$Name) {
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        Fail "$Name is required for release smoke verification"
    }
}

function Assert-DarwinArm64GoHost {
    $goos = (& go env GOOS).Trim()
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to resolve Go host GOOS"
    }
    $goarch = (& go env GOARCH).Trim()
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to resolve Go host GOARCH"
    }
    Write-Host "Go host tuple: $goos/$goarch"
    if ($goos -ne "darwin" -or $goarch -ne "arm64") {
        Fail "release smoke must run on darwin/arm64; got $goos/$goarch"
    }
}

function Invoke-Checked([string]$Label, [scriptblock]$Block) {
    Write-Host "==> $Label"
    & $Block
    if ($LASTEXITCODE -ne 0) {
        Fail "$Label failed with exit code $LASTEXITCODE"
    }
}

function ConvertFrom-JsonOutput([string[]]$Lines, [string]$Label) {
    $text = ($Lines -join "`n").Trim()
    if ($text -eq "") {
        Fail "$Label produced empty JSON output"
    }
    try {
        return $text | ConvertFrom-Json
    }
    catch {
        Fail "$Label produced invalid JSON: $($_.Exception.Message)`n$text"
    }
}

function Assert-OutsideRepo([string]$Path, [string]$RepoPath, [string]$Label) {
    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $fullRepo = [System.IO.Path]::GetFullPath($RepoPath).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
    $comparison = if ($IsWindows) { [System.StringComparison]::OrdinalIgnoreCase } else { [System.StringComparison]::Ordinal }
    if ($fullPath.Equals($fullRepo, $comparison) -or $fullPath.StartsWith($fullRepo + [System.IO.Path]::DirectorySeparatorChar, $comparison)) {
        Fail "$Label must live outside the repository: $fullPath is under $fullRepo"
    }
}

function Assert-SupportIsReady([object]$Entry, [string]$Label) {
    if (-not $Entry) {
        Fail "$Label was not present"
    }
    if (@("supported", "experimental") -notcontains $Entry.support) {
        Fail "$Label support must be supported or experimental, got $($Entry.support)"
    }
    if (@("supported", "experimental") -notcontains $Entry.code) {
        Fail "$Label code must be supported or experimental, got $($Entry.code)"
    }
    if (@("ok", "warn") -notcontains $Entry.status) {
        Fail "$Label status must be ok or warn, got $($Entry.status)"
    }
}

function Assert-DoctorCheckIsReady([object]$Entry, [string]$Label) {
    if (-not $Entry) {
        Fail "$Label was not present"
    }
    if (@("supported", "experimental") -notcontains $Entry.code) {
        Fail "$Label code must be supported or experimental, got $($Entry.code)"
    }
    if (@("ok", "warn") -notcontains $Entry.status) {
        Fail "$Label status must be ok or warn, got $($Entry.status)"
    }
}

function Get-PlatformAssetName([string]$SelectedVersion) {
    return "loopcoder_${SelectedVersion}_darwin_arm64.tar.gz"
}

function Expand-LoopcoderArchive([string]$Archive, [string]$Destination) {
    if (-not $Archive.EndsWith("_darwin_arm64.tar.gz", [System.StringComparison]::Ordinal)) {
        Fail "unsupported release archive format: $(Split-Path -Leaf $Archive)"
    }
    tar -xzf $Archive -C $Destination
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to extract $Archive"
    }
}

function Get-SHA256([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) {
        Fail "expected file does not exist: $Path"
    }
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Get-DarwinFileMode([string]$Path) {
    $mode = (& stat -f "%Lp" $Path).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($mode)) {
        Fail "failed to inspect file mode for $Path"
    }
    return $mode
}

function Assert-CandidateInstalledBinary([string]$BinaryPath, [string]$ExpectedHash, [string]$ExpectedVersion, [string]$Label) {
    if (-not (Test-Path -LiteralPath $BinaryPath)) {
        Fail "$Label binary was not installed at $BinaryPath"
    }
    $actualHash = Get-SHA256 $BinaryPath
    if ($actualHash -ne $ExpectedHash) {
        Fail "$Label binary hash mismatch: expected staged candidate hash $ExpectedHash, got $actualHash"
    }
    $mode = Get-DarwinFileMode $BinaryPath
    if ($mode -ne "755") {
        Fail "$Label binary mode is $mode, want 755"
    }

    $versionOutput = @(& $BinaryPath version)
    $versionOutput | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) {
        Fail "$Label binary version command failed"
    }
    $versionLines = @($versionOutput | Where-Object { $_ -match '(^|\s)version=' })
    if ($versionLines.Count -ne 1) {
        Fail "$Label binary emitted $($versionLines.Count) version lines; want exactly one"
    }
    $versionLine = [string]$versionLines[0]
    $versionPattern = "(^|\s)version=$([regex]::Escape($ExpectedVersion))(\s|$)"
    if ($versionLine -notmatch $versionPattern -or $versionLine -match "(^|\s)(commit|date)=unknown(\s|$)") {
        Fail "$Label binary did not report $ExpectedVersion with non-placeholder commit/date"
    }
}

function Assert-AssetNameSet([string[]]$Names, [string[]]$Expected, [string]$Label) {
    $actual = @($Names | Sort-Object)
    $want = @($Expected | Sort-Object)
    if ($actual.Count -ne $want.Count) {
        Fail "$Label asset inventory contains $($actual.Count) files; want $($want.Count): $($actual -join ', ')"
    }
    for ($i = 0; $i -lt $want.Count; $i++) {
        if ($actual[$i] -ne $want[$i]) {
            Fail "$Label asset inventory mismatch; got $($actual -join ', '), want $($want -join ', ')"
        }
    }
}

function Assert-RemoteReleaseAssetInventory([string]$SelectedTag, [string]$SelectedAsset, [string]$Label) {
    $inventoryOutput = @(& gh release view $SelectedTag --repo $Repo --json assets)
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to inspect $Label release asset inventory"
    }
    $inventory = ConvertFrom-JsonOutput $inventoryOutput "$Label release asset inventory"
    $assetNames = @($inventory.assets | ForEach-Object { [string]$_.name })
    Assert-AssetNameSet $assetNames @($SelectedAsset, "SHA256SUMS", "SHA256SUMS.sigstore") "$Label remote release"
}

function Assert-LocalReleaseAssetInventory([string]$ReleaseDir, [string]$SelectedAsset, [string]$Label) {
    $assetNames = @(Get-ChildItem -LiteralPath $ReleaseDir -File | ForEach-Object { $_.Name })
    Assert-AssetNameSet $assetNames @($SelectedAsset, "SHA256SUMS", "SHA256SUMS.sigstore") "$Label downloaded release"
}

function Get-ReleaseArchive([string]$SelectedVersion, [string]$Label, [bool]$RequireExactRemoteInventory = $false) {
    $selectedTag = if ($SelectedVersion.StartsWith("v")) { $SelectedVersion } else { "v$SelectedVersion" }
    $selectedPlainVersion = $selectedTag.TrimStart("v")
    $selectedAsset = Get-PlatformAssetName $selectedPlainVersion
    $identity = "$GitHubBaseUrl/$Repo/.github/workflows/release.yml@refs/tags/$selectedTag"
    $issuer = "https://token.actions.githubusercontent.com"
    $releaseDir = Join-Path $tmp ("release-" + $selectedPlainVersion)
    New-Item -ItemType Directory -Path $releaseDir | Out-Null

    Invoke-Checked "check $Label release exists" {
        gh release view $selectedTag --repo $Repo | Out-Host
    }
    if ($RequireExactRemoteInventory) {
        Assert-RemoteReleaseAssetInventory -SelectedTag $selectedTag -SelectedAsset $selectedAsset -Label $Label
    }

    $archivePath = Join-Path $releaseDir $selectedAsset
    $sumsPath = Join-Path $releaseDir "SHA256SUMS"
    $signaturePath = Join-Path $releaseDir "SHA256SUMS.sigstore"

    Invoke-Checked "download $Label release assets" {
        gh release download $selectedTag --repo $Repo --dir $releaseDir --clobber --pattern $selectedAsset --pattern "SHA256SUMS" --pattern "SHA256SUMS.sigstore" | Out-Host
    }
    foreach ($requiredPath in @($archivePath, $sumsPath, $signaturePath)) {
        if (-not (Test-Path -LiteralPath $requiredPath)) {
            Fail "downloaded $Label release did not include $(Split-Path -Leaf $requiredPath)"
        }
    }
    Assert-LocalReleaseAssetInventory -ReleaseDir $releaseDir -SelectedAsset $selectedAsset -Label $Label

    Invoke-Checked "verify $Label SHA256SUMS signature" {
        cosign verify-blob $sumsPath --bundle $signaturePath --certificate-identity $identity --certificate-oidc-issuer $issuer | Out-Host
    }

    $expectedLine = Get-Content -LiteralPath $sumsPath | Where-Object { $_ -match "\s$([regex]::Escape($selectedAsset))$" } | Select-Object -First 1
    if (-not $expectedLine) {
        Fail "SHA256SUMS does not contain $selectedAsset"
    }
    $expectedHash = ($expectedLine -split "\s+")[0].ToLowerInvariant()
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        Fail "checksum mismatch for $selectedAsset`: expected $expectedHash, got $actualHash"
    }

    return [pscustomobject]@{
        Tag = $selectedTag
        PlainVersion = $selectedPlainVersion
        Asset = $selectedAsset
        ArchivePath = $archivePath
        SumsPath = $sumsPath
        SignaturePath = $signaturePath
    }
}

function Get-FreeLoopbackPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Start-LocalReleaseApi([object]$Release) {
    $port = Get-FreeLoopbackPort
    $baseUrl = "http://127.0.0.1:$port/"
    $assetPaths = @{
        $Release.Asset = $Release.ArchivePath
        "SHA256SUMS" = $Release.SumsPath
        "SHA256SUMS.sigstore" = $Release.SignaturePath
    }
    $job = Start-Job -Name "loopcoder-release-smoke-api-$port" -ScriptBlock {
        param(
            [string]$Prefix,
            [string]$RepoName,
            [string]$TagName,
            [hashtable]$Assets
        )

        Set-StrictMode -Version Latest
        $ErrorActionPreference = "Stop"
        $listener = [System.Net.HttpListener]::new()
        $listener.Prefixes.Add($Prefix)
        $listener.Start()

        function Write-Bytes([System.Net.HttpListenerResponse]$Response, [int]$StatusCode, [string]$ContentType, [byte[]]$Bytes) {
            $Response.StatusCode = $StatusCode
            $Response.ContentType = $ContentType
            $Response.ContentLength64 = $Bytes.Length
            $Response.OutputStream.Write($Bytes, 0, $Bytes.Length)
            $Response.OutputStream.Close()
        }

        function Write-Json([System.Net.HttpListenerResponse]$Response, [int]$StatusCode, [object]$Payload) {
            $json = $Payload | ConvertTo-Json -Depth 8 -Compress
            Write-Bytes $Response $StatusCode "application/json" ([System.Text.Encoding]::UTF8.GetBytes($json))
        }

        try {
            while ($listener.IsListening) {
                try {
                    $context = $listener.GetContext()
                }
                catch [System.Net.HttpListenerException] {
                    break
                }
                catch [System.ObjectDisposedException] {
                    break
                }

                try {
                    $path = [System.Uri]::UnescapeDataString($context.Request.Url.AbsolutePath)
                    if ($context.Request.HttpMethod -ne "GET") {
                        Write-Json $context.Response 405 @{ message = "method not allowed" }
                        continue
                    }
                    if ($path -eq "/__health") {
                        Write-Json $context.Response 200 @{ status = "ok" }
                        continue
                    }
                    if ($path -eq "/__stop") {
                        Write-Json $context.Response 200 @{ status = "stopping" }
                        break
                    }

                    $releasePath = "/repos/$RepoName/releases/tags/$TagName"
                    if ($path -eq $releasePath) {
                        $assetPayload = @(
                            foreach ($name in $Assets.Keys) {
                                @{
                                    name = $name
                                    browser_download_url = "${Prefix}assets/$([System.Uri]::EscapeDataString($name))"
                                }
                            }
                        )
                        Write-Json $context.Response 200 @{
                            tag_name = $TagName
                            draft = $false
                            prerelease = $false
                            assets = $assetPayload
                        }
                        continue
                    }

                    $downloadPrefix = "/$RepoName/releases/download/$TagName/"
                    if ($path.StartsWith($downloadPrefix, [System.StringComparison]::Ordinal)) {
                        $assetName = $path.Substring($downloadPrefix.Length)
                        if ($Assets.ContainsKey($assetName) -and (Test-Path -LiteralPath $Assets[$assetName])) {
                            $stream = [System.IO.File]::OpenRead($Assets[$assetName])
                            try {
                                $context.Response.StatusCode = 200
                                $context.Response.ContentType = "application/octet-stream"
                                $context.Response.ContentLength64 = $stream.Length
                                $stream.CopyTo($context.Response.OutputStream)
                                $context.Response.OutputStream.Close()
                            }
                            finally {
                                $stream.Dispose()
                            }
                            continue
                        }
                    }

                    if ($path.StartsWith("/assets/", [System.StringComparison]::Ordinal)) {
                        $assetName = $path.Substring("/assets/".Length)
                        if ($Assets.ContainsKey($assetName) -and (Test-Path -LiteralPath $Assets[$assetName])) {
                            $stream = [System.IO.File]::OpenRead($Assets[$assetName])
                            try {
                                $context.Response.StatusCode = 200
                                $context.Response.ContentType = "application/octet-stream"
                                $context.Response.ContentLength64 = $stream.Length
                                $stream.CopyTo($context.Response.OutputStream)
                                $context.Response.OutputStream.Close()
                            }
                            finally {
                                $stream.Dispose()
                            }
                            continue
                        }
                    }

                    Write-Json $context.Response 404 @{ message = "not found" }
                }
                catch {
                    try {
                        Write-Json $context.Response 500 @{ message = $_.Exception.Message }
                    }
                    catch {
                    }
                }
            }
        }
        finally {
            $listener.Stop()
            $listener.Close()
        }
    } -ArgumentList $baseUrl, $Repo, $Release.Tag, $assetPaths

    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        if (@("Completed", "Failed", "Stopped") -contains $job.State) {
            $jobOutput = Receive-Job -Job $job -Keep -ErrorAction SilentlyContinue | Out-String
            Fail "local release API exited before becoming ready: $($job.State)`n$jobOutput"
        }
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri ($baseUrl + "__health") -TimeoutSec 1
            if ($response.StatusCode -eq 200) {
                return [pscustomobject]@{
                    Url = $baseUrl.TrimEnd("/")
                    Job = $job
                }
            }
        }
        catch {
            Start-Sleep -Milliseconds 100
        }
    }

    Stop-Job -Job $job -ErrorAction SilentlyContinue
    Remove-Job -Job $job -Force -ErrorAction SilentlyContinue
    Fail "local release API did not become ready at $baseUrl"
}

function Stop-LocalReleaseApi([object]$Server) {
    if (-not $Server) {
        return
    }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri ($Server.Url + "/__stop") -TimeoutSec 1 | Out-Null
    }
    catch {
    }
    try {
        Wait-Job -Job $Server.Job -Timeout 5 | Out-Null
    }
    catch {
    }
    if ($Server.Job.State -eq "Running") {
        Stop-Job -Job $Server.Job -ErrorAction SilentlyContinue
    }
    Remove-Job -Job $Server.Job -Force -ErrorAction SilentlyContinue
}

function Invoke-WithMockReleaseApi([object]$Server, [scriptblock]$Block) {
    $previousBaseUrl = [Environment]::GetEnvironmentVariable("GITHUB_BASE_URL", "Process")
    $previousApiUrl = [Environment]::GetEnvironmentVariable("GITHUB_API_URL", "Process")
    $previousCosignIdentity = [Environment]::GetEnvironmentVariable("LOOPCODER_COSIGN_IDENTITY", "Process")
    $releaseIdentity = "$GitHubBaseUrl/$Repo/.github/workflows/release.yml@refs/tags/$tag"
    try {
        [Environment]::SetEnvironmentVariable("GITHUB_BASE_URL", $Server.Url, "Process")
        [Environment]::SetEnvironmentVariable("GITHUB_API_URL", $Server.Url, "Process")
        [Environment]::SetEnvironmentVariable("LOOPCODER_COSIGN_IDENTITY", $releaseIdentity, "Process")
        & $Block
    }
    finally {
        [Environment]::SetEnvironmentVariable("GITHUB_BASE_URL", $previousBaseUrl, "Process")
        [Environment]::SetEnvironmentVariable("GITHUB_API_URL", $previousApiUrl, "Process")
        [Environment]::SetEnvironmentVariable("LOOPCODER_COSIGN_IDENTITY", $previousCosignIdentity, "Process")
    }
}

function Invoke-WithEnvironment([hashtable]$Values, [scriptblock]$Block) {
    $previous = @{}
    foreach ($key in $Values.Keys) {
        $previous[$key] = [Environment]::GetEnvironmentVariable($key, "Process")
        [Environment]::SetEnvironmentVariable($key, [string]$Values[$key], "Process")
    }
    try {
        & $Block
    }
    finally {
        foreach ($key in $Values.Keys) {
            [Environment]::SetEnvironmentVariable($key, $previous[$key], "Process")
        }
    }
}

function CandidateInstallEnvironment([object]$Server, [string]$InstallDir, [string]$InstallTmpDir, [string]$PathPrefix = "") {
    $releaseIdentity = "$GitHubBaseUrl/$Repo/.github/workflows/release.yml@refs/tags/$tag"
    $pathParts = @()
    if (-not [string]::IsNullOrWhiteSpace($PathPrefix)) {
        $pathParts += $PathPrefix
    }
    $pathParts += $InstallDir
    $pathParts += [Environment]::GetEnvironmentVariable("PATH", "Process")
    $pathValue = $pathParts -join ":"
    $installHome = Join-Path (Split-Path -Parent $InstallTmpDir) "home"
    return @{
        "GITHUB_BASE_URL" = $Server.Url
        "GITHUB_API_URL" = $Server.Url
        "LOOPCODER_COSIGN_IDENTITY" = $releaseIdentity
        "LOOPCODER_INSTALL_REPO" = $Repo
        "LOOPCODER_UPGRADE_REPO" = $Repo
        "LOOPCODER_INSTALL_DIR" = $InstallDir
        "LOOPCODER_INSTALL_OS" = "darwin"
        "LOOPCODER_INSTALL_ARCH" = "arm64"
        "TMPDIR" = $InstallTmpDir
        "HOME" = $installHome
        "PATH" = $pathValue
    }
}

function Invoke-CandidateInstall([object]$Server, [string]$InstallDir, [string]$InstallTmpDir, [string]$Label) {
    $installScript = Join-Path $scriptRoot "install.sh"
    if (-not (Test-Path -LiteralPath $installScript)) {
        Fail "install.sh not found at $installScript"
    }
    New-Item -ItemType Directory -Path $InstallTmpDir -Force | Out-Null
    $envValues = CandidateInstallEnvironment -Server $Server -InstallDir $InstallDir -InstallTmpDir $InstallTmpDir
    Invoke-WithEnvironment -Values $envValues -Block {
        & /bin/sh $installScript --version $plainVersion | Out-Host
        if ($LASTEXITCODE -ne 0) {
            Fail "$Label install.sh failed with exit code $LASTEXITCODE"
        }
    }
}

function Invoke-InterruptedCandidateInstallSeam([object]$Server, [string]$ExpectedHash) {
    $installScript = Join-Path $scriptRoot "install.sh"
    $caseDir = Join-Path $tmp "interrupted-install"
    $installDir = Join-Path $caseDir "bin"
    $installTmp = Join-Path $caseDir "tmp"
    $shimDir = Join-Path $caseDir "shim"
    New-Item -ItemType Directory -Path $installDir, $installTmp, $shimDir -Force | Out-Null

    $preexisting = Join-Path $installDir "loopcoder"
    @(
        '#!/bin/sh',
        'if [ "${1:-}" = "version" ] || [ "${1:-}" = "--version" ]; then',
        "  printf '%s\n' 'version=0.7.0 commit=preexisting date=2026-07-01T00:00:00Z'",
        '  exit 0',
        'fi',
        "printf '%s\n' 'preexisting loopcoder still usable'"
    ) | Set-Content -LiteralPath $preexisting -Encoding utf8
    chmod 755 $preexisting
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to make pre-existing rollback fixture executable"
    }
    $preexistingHash = Get-SHA256 $preexisting

    $mvReady = Join-Path $caseDir "mv.ready"
    $mvPidFile = Join-Path $caseDir "mv.pid"
    @(
        '#!/bin/sh',
        'set -eu',
        'printf ''%s\n'' "$$" >"$LOOPCODER_SMOKE_MV_PID"',
        'touch "$LOOPCODER_SMOKE_MV_READY"',
        'while :; do',
        '  sleep 1',
        'done'
    ) | Set-Content -LiteralPath (Join-Path $shimDir "mv") -Encoding utf8
    chmod 755 (Join-Path $shimDir "mv")
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to make mv interruption shim executable"
    }

    $stdoutPath = Join-Path $caseDir "install.stdout"
    $stderrPath = Join-Path $caseDir "install.stderr"
    $envValues = CandidateInstallEnvironment -Server $Server -InstallDir $installDir -InstallTmpDir $installTmp -PathPrefix $shimDir
    $envValues["LOOPCODER_SMOKE_MV_READY"] = $mvReady
    $envValues["LOOPCODER_SMOKE_MV_PID"] = $mvPidFile

    Invoke-WithEnvironment -Values $envValues -Block {
        $process = Start-Process -FilePath "/bin/sh" -ArgumentList @($installScript, "--version", $plainVersion) -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath -PassThru
        $deadline = [DateTime]::UtcNow.AddSeconds(20)
        while (-not (Test-Path -LiteralPath $mvReady) -and [DateTime]::UtcNow -lt $deadline) {
            if ($process.HasExited) {
                $stdout = if (Test-Path -LiteralPath $stdoutPath) { Get-Content -LiteralPath $stdoutPath -Raw } else { "" }
                $stderr = if (Test-Path -LiteralPath $stderrPath) { Get-Content -LiteralPath $stderrPath -Raw } else { "" }
                Fail "interrupted install exited before replacement seam was reached: exit=$($process.ExitCode)`nstdout:`n$stdout`nstderr:`n$stderr"
            }
            Start-Sleep -Milliseconds 100
        }
        if (-not (Test-Path -LiteralPath $mvReady)) {
            Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
            Wait-Process -Id $process.Id -Timeout 5 -ErrorAction SilentlyContinue
            Fail "interrupted install did not reach the atomic replacement seam"
        }

        $mvPid = (Get-Content -LiteralPath $mvPidFile -Raw).Trim()
        if ($mvPid -notmatch '^[0-9]+$') {
            Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
            Fail "interruption mv shim pid was not numeric: $mvPid"
        }
        $mvParent = (& ps -o ppid= -p $mvPid).Trim()
        if ($LASTEXITCODE -ne 0 -or $mvParent -ne [string]$process.Id) {
            Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
            Fail "interruption mv pid $mvPid parent was $mvParent, want installer pid $($process.Id)"
        }

        Stop-Process -Id ([int]$mvPid) -ErrorAction Stop
        Wait-Process -Id $process.Id -Timeout 10 -ErrorAction SilentlyContinue
        if (-not $process.HasExited) {
            Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
            Fail "interrupted install did not exit after replacement process was terminated"
        }
        if ($process.ExitCode -eq 0) {
            Fail "interrupted install unexpectedly succeeded"
        }
    }

    if ((Get-SHA256 $preexisting) -ne $preexistingHash) {
        Fail "interrupted install changed the pre-existing binary"
    }
    $preexistingOutput = @(& $preexisting version)
    if ($LASTEXITCODE -ne 0 -or -not (($preexistingOutput -join "`n") -match "version=0.7.0")) {
        Fail "pre-existing binary was not usable after interrupted install"
    }
    if (Get-ChildItem -LiteralPath $installDir -Force | Where-Object { $_.Name -like ".loopcoder.tmp.*" }) {
        Fail "interrupted install exposed a partial replacement in $installDir"
    }
    if (Get-ChildItem -LiteralPath $installTmp -Force | Where-Object { $_.Name -like "loopcoder-install.*" }) {
        Fail "interrupted install left installer temporary artifacts in $installTmp"
    }

    Invoke-CandidateInstall -Server $Server -InstallDir $installDir -InstallTmpDir $installTmp -Label "retry after interrupted install"
    Assert-CandidateInstalledBinary -BinaryPath (Join-Path $installDir "loopcoder") -ExpectedHash $ExpectedHash -ExpectedVersion $plainVersion -Label "retry after interrupted install"
}

Require-Command "go"
Assert-DarwinArm64GoHost
Require-Command "gh"
Require-Command "git"
Require-Command "cosign"
Require-Command "stat"

$tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
$plainVersion = $tag.TrimStart("v")
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("loopcoder-release-smoke-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
$oldHome = [Environment]::GetEnvironmentVariable("LOOPCODER_HOME", "Process")
$oldUpgradeRepo = [Environment]::GetEnvironmentVariable("LOOPCODER_UPGRADE_REPO", "Process")
$oldGitHubBaseUrl = [Environment]::GetEnvironmentVariable("GITHUB_BASE_URL", "Process")
$oldGitHubApiUrl = [Environment]::GetEnvironmentVariable("GITHUB_API_URL", "Process")
$oldCosignIdentity = [Environment]::GetEnvironmentVariable("LOOPCODER_COSIGN_IDENTITY", "Process")
$loopcoderHome = Join-Path $tmp "home"
New-Item -ItemType Directory -Path $loopcoderHome | Out-Null
[Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $loopcoderHome, "Process")
[Environment]::SetEnvironmentVariable("LOOPCODER_UPGRADE_REPO", $Repo, "Process")
[Environment]::SetEnvironmentVariable("GITHUB_BASE_URL", $GitHubBaseUrl, "Process")
[Environment]::SetEnvironmentVariable("GITHUB_API_URL", $GitHubApiUrl, "Process")
$scriptRoot = Split-Path -Parent $PSCommandPath
$sourceRepo = (Resolve-Path -LiteralPath (Join-Path $scriptRoot "..")).Path
$mockReleaseApi = $null

try {
    $release = Get-ReleaseArchive $plainVersion "candidate" $true
    $mockReleaseApi = Start-LocalReleaseApi $release

    $extractDir = Join-Path $tmp "extract"
    New-Item -ItemType Directory -Path $extractDir | Out-Null
    Expand-LoopcoderArchive -Archive $release.ArchivePath -Destination $extractDir
    $candidateBinary = Join-Path $extractDir "loopcoder"
    if (-not (Test-Path -LiteralPath $candidateBinary)) {
        Fail "archive did not contain loopcoder binary"
    }
    $candidateBinaryHash = Get-SHA256 $candidateBinary

    $installDir = Join-Path $tmp "fresh-install/bin"
    $installTmpDir = Join-Path $tmp "fresh-install/tmp"
    Invoke-CandidateInstall -Server $mockReleaseApi -InstallDir $installDir -InstallTmpDir $installTmpDir -Label "fresh install from staged candidate"
    $binary = Join-Path $installDir "loopcoder"
    Assert-CandidateInstalledBinary -BinaryPath $binary -ExpectedHash $candidateBinaryHash -ExpectedVersion $plainVersion -Label "fresh install from staged candidate"

    Invoke-InterruptedCandidateInstallSeam -Server $mockReleaseApi -ExpectedHash $candidateBinaryHash

    Invoke-Checked "verify source checkout has no tracked .loopcoder files" {
        $script:trackedLocalState = @(git -C $sourceRepo ls-files .loopcoder)
        if ($trackedLocalState.Count -gt 0) {
            $trackedLocalState | ForEach-Object { Write-Host $_ }
            exit 1
        }
    }

    $repoTmp = Join-Path $tmp "repo"
    New-Item -ItemType Directory -Path $repoTmp | Out-Null
    Invoke-Checked "initialize temporary git repo" {
        git -C $repoTmp init -b main | Out-Host
    }
    Invoke-Checked "loopcoder init" {
        & $binary init --repo $repoTmp | Out-Host
    }
    Invoke-Checked "loopcoder skill install" {
        & $binary skill install --repo $repoTmp | Out-Host
    }

    Invoke-Checked "loopcoder projects register" {
        $script:projectRegisterOutput = @(& $binary projects register --repo $repoTmp --format json)
        $script:projectRegisterOutput | ForEach-Object { Write-Host $_ }
    }
    $registered = ConvertFrom-JsonOutput $projectRegisterOutput "projects register"
    if (-not $registered.project.project_id) {
        Fail "projects register did not return a project_id"
    }
    $dbPath = Join-Path $loopcoderHome "data/loopcoder.db"
    if (-not (Test-Path -LiteralPath $dbPath)) {
        Fail "projects register did not create $dbPath"
    }
    Assert-OutsideRepo -Path $dbPath -RepoPath $repoTmp -Label "v0.7.0 database"
    $projectRoot = Join-Path $loopcoderHome ("projects/" + $registered.project.project_id)
    foreach ($payloadRel in @("runs", "relay", "recovery", "audit")) {
        $payloadPath = Join-Path $projectRoot $payloadRel
        if (-not (Test-Path -LiteralPath $payloadPath)) {
            Fail "projects register did not create project payload directory $payloadPath"
        }
        Assert-OutsideRepo -Path $payloadPath -RepoPath $repoTmp -Label "v0.7.0 project payload $payloadRel"
    }
    foreach ($homeRel in @("logs", "tmp")) {
        $homeRuntimePath = Join-Path $loopcoderHome $homeRel
        if (-not (Test-Path -LiteralPath $homeRuntimePath)) {
            Fail "projects register did not create home runtime directory $homeRuntimePath"
        }
        Assert-OutsideRepo -Path $homeRuntimePath -RepoPath $repoTmp -Label "v0.7.0 home runtime $homeRel"
    }

    Invoke-Checked "loopcoder projects show" {
        $script:projectShowOutput = @(& $binary projects show --repo $repoTmp --format json)
        $script:projectShowOutput | ForEach-Object { Write-Host $_ }
    }
    $shown = ConvertFrom-JsonOutput $projectShowOutput "projects show"
    if ($shown.project.project_id -ne $registered.project.project_id) {
        Fail "projects show returned $($shown.project.project_id), want $($registered.project.project_id)"
    }

    Invoke-Checked "loopcoder projects list" {
        $script:projectListOutput = @(& $binary projects list --format json)
        $script:projectListOutput | ForEach-Object { Write-Host $_ }
    }
    $listed = ConvertFrom-JsonOutput $projectListOutput "projects list"
    if (@($listed.projects | Where-Object { $_.project_id -eq $registered.project.project_id }).Count -ne 1) {
        Fail "projects list did not include registered project $($registered.project.project_id)"
    }

    $legacyWorkerDir = Join-Path $repoTmp ".loopcoder/runs/run-release-smoke/workers"
    New-Item -ItemType Directory -Path $legacyWorkerDir | Out-Null
    @{
        version = 1
        job_id = "job-release-smoke-1"
        issue = 655
        attempt = 1
        provider = "codex"
        status = "succeeded"
        started_at = "2026-07-10T00:00:00Z"
        report = @{
            role = "worker"
            provider = "codex"
            model = "gpt-5.5"
            model_source = "parsed"
            effort = "high"
            permission = "write"
            action = "release smoke migration fixture"
            exit_code = 0
            started_at = "2026-07-10T00:00:00Z"
            ended_at = "2026-07-10T00:00:01Z"
            duration_ms = 1000
            usage = @{ total_tokens = 655 }
            verified = $true
            work_id = "run-release-smoke"
            issue = 655
            branch = "loop/issue-655"
        }
    } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $legacyWorkerDir "job-release-smoke-1.attempt.json") -Encoding utf8

    Invoke-Checked "loopcoder migrate local-state dry run" {
        $script:migrationOutput = @(& $binary migrate local-state --repo $repoTmp --dry-run --format json)
        $script:migrationOutput | ForEach-Object { Write-Host $_ }
    }
    $migration = ConvertFrom-JsonOutput $migrationOutput "migrate local-state --dry-run"
    if (-not $migration.dry_run -or $migration.status -notmatch "^dry-run" -or $migration.scanned_count -lt 1 -or $migration.report_count -lt 1) {
        Fail "migration dry run did not report the fixture as importable: $($migrationOutput -join "`n")"
    }

    $doctorJson = @(& $binary doctor --repo $repoTmp --format json)
    $doctorPayload = ConvertFrom-JsonOutput $doctorJson "doctor --format json"
    $doctorJson | ForEach-Object { Write-Host $_ }
    if ($doctorPayload.exit_code -ne 0) {
        Fail "doctor JSON reported exit_code=$($doctorPayload.exit_code)"
    }
    if ($doctorPayload.runtime.database.path -ne $dbPath.Replace("\", "/") -and $doctorPayload.runtime.database.path -ne $dbPath) {
        Fail "doctor JSON reported unexpected database path: $($doctorPayload.runtime.database.path)"
    }
    if (-not $doctorPayload.runtime.database.exists -or $doctorPayload.runtime.database.status -ne "ok") {
        Fail "doctor JSON did not report healthy v0.7.0 storage"
    }
    if (-not $doctorPayload.runtime.project_registry.registered -or $doctorPayload.runtime.project_registry.project_id -ne $registered.project.project_id) {
        Fail "doctor JSON did not report the registered smoke project"
    }
    if (-not $doctorPayload.runtime.project_registry.payload_root) {
        Fail "doctor JSON did not report the registered project payload root"
    }
    foreach ($runtimePath in @(
        $doctorPayload.runtime.project_registry.payload_root,
        $doctorPayload.runtime.project_registry.runs_root,
        $doctorPayload.runtime.project_registry.relay_root,
        $doctorPayload.runtime.project_registry.recovery_root,
        $doctorPayload.runtime.project_registry.audit_root,
        $doctorPayload.runtime.project_registry.logs_root,
        $doctorPayload.runtime.project_registry.tmp_root
    )) {
        if ([string]::IsNullOrWhiteSpace($runtimePath)) {
            Fail "doctor JSON omitted a v0.7.0 runtime payload path"
        }
        Assert-OutsideRepo -Path $runtimePath -RepoPath $repoTmp -Label "doctor runtime payload path"
    }
    if ($doctorPayload.runtime.project_registry.fallback_mode -ne "registered-global") {
        Fail "doctor JSON reported fallback_mode=$($doctorPayload.runtime.project_registry.fallback_mode), want registered-global"
    }
    $defaultWorkerSmoke = @($doctorPayload.provider_compatibility | Where-Object {
        $_.provider -eq "codex" -and $_.role -eq "worker"
    })
    if ($defaultWorkerSmoke.Count -lt 1) {
        Fail "doctor JSON did not include default codex worker provider compatibility"
    }
    Assert-SupportIsReady $defaultWorkerSmoke[0] "doctor JSON codex worker provider compatibility"
    $defaultWorkerCheck = @($doctorPayload.checks | Where-Object {
        $_.name -eq "provider compatibility codex worker"
    })
    if ($defaultWorkerCheck.Count -ne 1) {
        Fail "doctor JSON did not include selected default codex worker compatibility check"
    }
    Assert-DoctorCheckIsReady $defaultWorkerCheck[0] "doctor JSON codex worker compatibility check"
    Invoke-Checked "loopcoder report" {
        $script:reportOutput = @(& $binary report --repo $repoTmp --format json)
        $script:reportOutput | ForEach-Object { Write-Host $_ }
    }
    $report = ConvertFrom-JsonOutput $reportOutput "report --format json"
    if (@($report.records | Where-Object { $_.run_id -eq "run-release-smoke" -and $_.report.role -eq "worker" }).Count -lt 1) {
        Fail "report JSON did not include the release smoke worker fixture"
    }
    Invoke-Checked "upgrade already-latest $plainVersion" {
        Invoke-WithMockReleaseApi -Server $mockReleaseApi -Block {
            $script:upgradeOutput = @(& $binary upgrade --version $plainVersion)
            $script:upgradeOutput | ForEach-Object { Write-Host $_ }
        }
    }
    if (-not (($upgradeOutput -join "`n") -match "Already latest|already latest|no download needed")) {
        Fail "upgrade did not recognize $plainVersion as already latest"
    }

    $selfBootstrapScript = Join-Path $scriptRoot "self-bootstrap-smoke.ps1"
    if (-not (Test-Path -LiteralPath $selfBootstrapScript)) {
        Fail "self-bootstrap smoke script not found at $selfBootstrapScript"
    }
    Invoke-Checked "v0.7.0 self-bootstrap acceptance smoke" {
        & $selfBootstrapScript -Repo $sourceRepo -Binary $binary -Version $plainVersion | Out-Host
    }

    if (-not [string]::IsNullOrWhiteSpace($PreviousVersion) -and $PreviousVersion.TrimStart("v") -ne $plainVersion) {
        $previous = Get-ReleaseArchive $PreviousVersion "previous $PreviousVersion"
        $previousExtractDir = Join-Path $tmp "previous-extract"
        New-Item -ItemType Directory -Path $previousExtractDir | Out-Null
        Expand-LoopcoderArchive -Archive $previous.ArchivePath -Destination $previousExtractDir
        $previousBinary = Join-Path $previousExtractDir "loopcoder"
        if (-not (Test-Path -LiteralPath $previousBinary)) {
            Fail "previous archive did not contain loopcoder binary"
        }
        Invoke-Checked "upgrade from $($previous.PlainVersion) to $plainVersion" {
            Invoke-WithMockReleaseApi -Server $mockReleaseApi -Block {
                $script:previousUpgradeOutput = @(& $previousBinary upgrade --version $plainVersion)
                $script:previousUpgradeOutput | ForEach-Object { Write-Host $_ }
            }
        }
        if (-not (($previousUpgradeOutput -join "`n") -match "After: .*version=v?$([regex]::Escape($plainVersion))|Installed versioned binary:")) {
            Fail "previous-version upgrade output did not show installation of $plainVersion"
        }
        $upgradedStableBinary = Join-Path $loopcoderHome "bin/loopcoder"
        Assert-CandidateInstalledBinary -BinaryPath $upgradedStableBinary -ExpectedHash $candidateBinaryHash -ExpectedVersion $plainVersion -Label "previous-version upgrade from staged candidate"
    }

    Write-Host "release smoke verification passed for $tag"
}
finally {
    Stop-LocalReleaseApi $mockReleaseApi
    [Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $oldHome, "Process")
    [Environment]::SetEnvironmentVariable("LOOPCODER_UPGRADE_REPO", $oldUpgradeRepo, "Process")
    [Environment]::SetEnvironmentVariable("GITHUB_BASE_URL", $oldGitHubBaseUrl, "Process")
    [Environment]::SetEnvironmentVariable("GITHUB_API_URL", $oldGitHubApiUrl, "Process")
    [Environment]::SetEnvironmentVariable("LOOPCODER_COSIGN_IDENTITY", $oldCosignIdentity, "Process")
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
