#!/usr/bin/env pwsh
param(
    [string]$Version = "0.6.1",
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

Require-Command "gh"
Require-Command "git"
Require-Command "cosign"

$tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
$plainVersion = $tag.TrimStart("v")
$asset = Get-PlatformAssetName $plainVersion
$releaseUrl = "$GitHubBaseUrl/$Repo/releases/download/$tag"
$identity = "$GitHubBaseUrl/$Repo/.github/workflows/release.yml@refs/tags/$tag"
$issuer = "https://token.actions.githubusercontent.com"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("loopcoder-release-smoke-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    Invoke-Checked "check release exists" {
        gh release view $tag --repo $Repo | Out-Host
    }

    $archivePath = Join-Path $tmp $asset
    $sumsPath = Join-Path $tmp "SHA256SUMS"
    $signaturePath = Join-Path $tmp "SHA256SUMS.sigstore"

    Invoke-WebRequest -Uri "$releaseUrl/$asset" -OutFile $archivePath
    Invoke-WebRequest -Uri "$releaseUrl/SHA256SUMS" -OutFile $sumsPath
    Invoke-WebRequest -Uri "$releaseUrl/SHA256SUMS.sigstore" -OutFile $signaturePath

    Invoke-Checked "verify SHA256SUMS signature" {
        cosign verify-blob $sumsPath --bundle $signaturePath --certificate-identity $identity --certificate-oidc-issuer $issuer | Out-Host
    }

    $expectedLine = Get-Content -LiteralPath $sumsPath | Where-Object { $_ -match "\s$([regex]::Escape($asset))$" } | Select-Object -First 1
    if (-not $expectedLine) {
        Fail "SHA256SUMS does not contain $asset"
    }
    $expectedHash = ($expectedLine -split "\s+")[0].ToLowerInvariant()
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        Fail "checksum mismatch for $asset`: expected $expectedHash, got $actualHash"
    }

    $extractDir = Join-Path $tmp "extract"
    New-Item -ItemType Directory -Path $extractDir | Out-Null
    Expand-LoopcoderArchive -Archive $archivePath -Destination $extractDir
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
    $doctorJson = & $binary doctor --repo $repoTmp --format json
    $doctorPayload = $doctorJson | ConvertFrom-Json
    Write-Host $doctorJson
    $defaultWorkerSmoke = @($doctorPayload.provider_compatibility | Where-Object {
        $_.provider -eq "codex" -and $_.role -eq "worker" -and $_.support -eq "supported"
    })
    if ($defaultWorkerSmoke.Count -lt 1) {
        Fail "doctor JSON did not include supported default codex worker provider compatibility"
    }
    $defaultWorkerCheck = @($doctorPayload.checks | Where-Object {
        $_.name -eq "provider compatibility codex worker" -and $_.code -eq "supported" -and $_.status -eq "ok"
    })
    if ($defaultWorkerCheck.Count -ne 1) {
        Fail "doctor JSON did not include an ok selected default codex worker compatibility check"
    }
    Invoke-Checked "loopcoder report" {
        & $binary report --repo $repoTmp | Out-Host
    }
    $upgradeOutput = & $binary upgrade --version $plainVersion
    Write-Host $upgradeOutput
    if ($upgradeOutput -notmatch "Already latest|already latest|no download needed") {
        Fail "upgrade did not recognize $plainVersion as already latest"
    }

    Write-Host "release smoke verification passed for $tag"
}
finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
