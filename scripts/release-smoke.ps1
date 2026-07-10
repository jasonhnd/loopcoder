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

function Get-PlatformAssetName([string]$SelectedVersion) {
    $goos = switch ($PSVersionTable.Platform) {
        "Unix" {
            if ($IsMacOS) { "darwin" } else { "linux" }
        }
        default { "windows" }
    }
    if ($IsWindows) {
        $goos = "windows"
    }

    $arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture) {
        "X64" { "amd64" }
        "Arm64" { "arm64" }
        default { Fail "unsupported architecture: $([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture)" }
    }
    $ext = if ($goos -eq "windows") { "zip" } else { "tar.gz" }
    return "loopcoder_${SelectedVersion}_${goos}_${arch}.${ext}"
}

function Expand-LoopcoderArchive([string]$Archive, [string]$Destination) {
    if ($Archive.EndsWith(".zip", [System.StringComparison]::OrdinalIgnoreCase)) {
        Expand-Archive -LiteralPath $Archive -DestinationPath $Destination -Force
        return
    }
    tar -xzf $Archive -C $Destination
    if ($LASTEXITCODE -ne 0) {
        Fail "failed to extract $Archive"
    }
}

function Get-ReleaseArchive([string]$SelectedVersion, [string]$Label) {
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
    }
}

Require-Command "gh"
Require-Command "git"
Require-Command "cosign"

$tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
$plainVersion = $tag.TrimStart("v")
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("loopcoder-release-smoke-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
$oldHome = [Environment]::GetEnvironmentVariable("LOOPCODER_HOME", "Process")
$oldUpgradeRepo = [Environment]::GetEnvironmentVariable("LOOPCODER_UPGRADE_REPO", "Process")
$oldGitHubBaseUrl = [Environment]::GetEnvironmentVariable("GITHUB_BASE_URL", "Process")
$oldGitHubApiUrl = [Environment]::GetEnvironmentVariable("GITHUB_API_URL", "Process")
$loopcoderHome = Join-Path $tmp "home"
New-Item -ItemType Directory -Path $loopcoderHome | Out-Null
[Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $loopcoderHome, "Process")
[Environment]::SetEnvironmentVariable("LOOPCODER_UPGRADE_REPO", $Repo, "Process")
[Environment]::SetEnvironmentVariable("GITHUB_BASE_URL", $GitHubBaseUrl, "Process")
[Environment]::SetEnvironmentVariable("GITHUB_API_URL", $GitHubApiUrl, "Process")
$scriptRoot = Split-Path -Parent $PSCommandPath
$sourceRepo = (Resolve-Path -LiteralPath (Join-Path $scriptRoot "..")).Path

try {
    $release = Get-ReleaseArchive $plainVersion "candidate"

    $extractDir = Join-Path $tmp "extract"
    New-Item -ItemType Directory -Path $extractDir | Out-Null
    Expand-LoopcoderArchive -Archive $release.ArchivePath -Destination $extractDir
    $binary = Join-Path $extractDir ($(if ($IsWindows) { "loopcoder.exe" } else { "loopcoder" }))
    if (-not (Test-Path -LiteralPath $binary)) {
        Fail "archive did not contain loopcoder binary"
    }

    $versionOutput = @(& $binary version)
    $versionOutput | ForEach-Object { Write-Host $_ }
    $versionLines = @($versionOutput | Where-Object { $_ -match '(^|\s)version=' })
    if ($versionLines.Count -ne 1) {
        Fail "downloaded binary emitted $($versionLines.Count) version lines; want exactly one"
    }
    $versionLine = [string]$versionLines[0]
    $versionPattern = "(^|\s)version=$([regex]::Escape($plainVersion))(\s|$)"
    if ($versionLine -notmatch $versionPattern -or $versionLine -match "(^|\s)(commit|date)=unknown(\s|$)") {
        Fail "downloaded binary did not report $plainVersion with non-placeholder commit/date"
    }

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
    Assert-SupportIsReady $defaultWorkerCheck[0] "doctor JSON codex worker compatibility check"
    Invoke-Checked "loopcoder report" {
        $script:reportOutput = @(& $binary report --repo $repoTmp --format json)
        $script:reportOutput | ForEach-Object { Write-Host $_ }
    }
    $report = ConvertFrom-JsonOutput $reportOutput "report --format json"
    if (@($report.records | Where-Object { $_.run_id -eq "run-release-smoke" -and $_.report.role -eq "worker" }).Count -lt 1) {
        Fail "report JSON did not include the release smoke worker fixture"
    }
    $upgradeOutput = & $binary upgrade --version $plainVersion
    Write-Host $upgradeOutput
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
        $previousBinary = Join-Path $previousExtractDir ($(if ($IsWindows) { "loopcoder.exe" } else { "loopcoder" }))
        if (-not (Test-Path -LiteralPath $previousBinary)) {
            Fail "previous archive did not contain loopcoder binary"
        }
        Invoke-Checked "upgrade from $($previous.PlainVersion) to $plainVersion" {
            $script:previousUpgradeOutput = @(& $previousBinary upgrade --version $plainVersion)
            $script:previousUpgradeOutput | ForEach-Object { Write-Host $_ }
        }
        if (-not (($previousUpgradeOutput -join "`n") -match "After: .*version=v?$([regex]::Escape($plainVersion))|Installed versioned binary:")) {
            Fail "previous-version upgrade output did not show installation of $plainVersion"
        }
    }

    Write-Host "release smoke verification passed for $tag"
}
finally {
    [Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $oldHome, "Process")
    [Environment]::SetEnvironmentVariable("LOOPCODER_UPGRADE_REPO", $oldUpgradeRepo, "Process")
    [Environment]::SetEnvironmentVariable("GITHUB_BASE_URL", $oldGitHubBaseUrl, "Process")
    [Environment]::SetEnvironmentVariable("GITHUB_API_URL", $oldGitHubApiUrl, "Process")
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
