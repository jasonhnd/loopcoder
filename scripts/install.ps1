[CmdletBinding()]
param(
    [Alias("v")]
    [string]$Version = $env:LOOPCODER_VERSION
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"

$Repo = if ($env:LOOPCODER_INSTALL_REPO) { $env:LOOPCODER_INSTALL_REPO } else { "jasonhnd/loopcoder" }
$GitHubBaseUrl = if ($env:GITHUB_BASE_URL) { $env:GITHUB_BASE_URL.TrimEnd("/") } else { "https://github.com" }
$GitHubApiUrl = if ($env:GITHUB_API_URL) { $env:GITHUB_API_URL.TrimEnd("/") } else { "https://api.github.com" }
$BinDir = if ($env:LOOPCODER_INSTALL_DIR) { $env:LOOPCODER_INSTALL_DIR } else { Join-Path $HOME ".loopcoder\bin" }
$ChecksumSignatureAsset = "SHA256SUMS.sigstore"
$CosignIssuer = if ($env:LOOPCODER_COSIGN_ISSUER) { $env:LOOPCODER_COSIGN_ISSUER } else { "https://token.actions.githubusercontent.com" }
$Headers = @{
    "Accept" = "application/vnd.github+json"
    "User-Agent" = "loopcoder-install"
}

function Fail {
    param([string]$Message)
    throw "loopcoder install: $Message"
}

function Require-Command {
    param([string]$Name)

    if ($null -eq (Get-Command $Name -ErrorAction SilentlyContinue)) {
        Fail "$Name is required to verify the SHA256SUMS signature"
    }
}

function Resolve-LoopcoderTag {
    param([string]$RequestedVersion)

    if ([string]::IsNullOrWhiteSpace($RequestedVersion) -or $RequestedVersion -eq "latest") {
        try {
            $release = Invoke-RestMethod -Uri "$GitHubApiUrl/repos/$Repo/releases/latest" -Headers $Headers -ErrorAction Stop
        } catch {
            Fail "failed to resolve latest release from GitHub. $($_.Exception.Message)"
        }

        if ([string]::IsNullOrWhiteSpace([string]$release.tag_name)) {
            Fail "GitHub latest release response did not include tag_name"
        }
        return [string]$release.tag_name
    }

    if ($RequestedVersion -match "^[vV]") {
        return $RequestedVersion
    }
    return "v$RequestedVersion"
}

function Get-WindowsArch {
    try {
        $runtimeArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        switch ($runtimeArch.ToUpperInvariant()) {
            "X64" { return "amd64" }
            "AMD64" { return "amd64" }
            "ARM64" { return "arm64" }
        }
    } catch {
        $runtimeArch = ""
    }

    foreach ($candidate in @($env:PROCESSOR_ARCHITEW6432, $env:PROCESSOR_ARCHITECTURE)) {
        if ([string]::IsNullOrWhiteSpace($candidate)) {
            continue
        }
        switch ($candidate.ToUpperInvariant()) {
            "AMD64" { return "amd64" }
            "X64" { return "amd64" }
            "ARM64" { return "arm64" }
        }
    }

    Fail "unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
}

function Download-File {
    param(
        [string]$Uri,
        [string]$OutFile,
        [string]$Label
    )

    try {
        Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing -Headers $Headers -ErrorAction Stop
    } catch {
        Fail "failed to download $Label from $Uri. $($_.Exception.Message)"
    }
}

function Get-ExpectedHash {
    param(
        [string]$SumsPath,
        [string]$ArchiveName
    )

    $escapedArchiveName = [regex]::Escape($ArchiveName)
    foreach ($line in Get-Content -LiteralPath $SumsPath) {
        if ($line -match "^(?<hash>[A-Fa-f0-9]{64})\s+\*?$escapedArchiveName\s*$") {
            return $Matches["hash"].ToLowerInvariant()
        }
    }

    Fail "SHA256SUMS does not contain $ArchiveName; release may be incomplete"
}

function Verify-ChecksumsSignature {
    param(
        [string]$SumsPath,
        [string]$SignaturePath,
        [string]$Identity,
        [string]$Issuer
    )

    $output = & cosign verify-blob $SumsPath --bundle $SignaturePath --certificate-identity $Identity --certificate-oidc-issuer $Issuer 2>&1
    if ($LASTEXITCODE -ne 0) {
        $detail = ($output | Out-String).Trim()
        if ([string]::IsNullOrWhiteSpace($detail)) {
            Fail "failed to verify SHA256SUMS signature with cosign identity $Identity and issuer $Issuer"
        }
        Fail "failed to verify SHA256SUMS signature with cosign identity $Identity and issuer $Issuer. $detail"
    }
}

function Test-PathListContains {
    param(
        [AllowNull()][string]$PathValue,
        [string]$Directory
    )

    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return $false
    }

    $target = [System.IO.Path]::GetFullPath($Directory).TrimEnd("\")
    foreach ($entry in ($PathValue -split ";")) {
        if ([string]::IsNullOrWhiteSpace($entry)) {
            continue
        }
        try {
            $expanded = [Environment]::ExpandEnvironmentVariables($entry)
            $candidate = [System.IO.Path]::GetFullPath($expanded).TrimEnd("\")
        } catch {
            $candidate = $entry.TrimEnd("\")
        }
        if ([string]::Equals($candidate, $target, [StringComparison]::OrdinalIgnoreCase)) {
            return $true
        }
    }

    return $false
}

function Ensure-UserPath {
    param([string]$Directory)

    if (-not (Test-PathListContains -PathValue $env:Path -Directory $Directory)) {
        $env:Path = "$Directory;$env:Path"
    }

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (Test-PathListContains -PathValue $userPath -Directory $Directory) {
        Write-Host "$Directory is already on the user PATH."
        return
    }

    try {
        if ([string]::IsNullOrWhiteSpace($userPath)) {
            $newUserPath = $Directory
        } else {
            $newUserPath = "$Directory;$userPath"
        }
        [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
        Write-Host "Added $Directory to the user PATH for new PowerShell sessions."
    } catch {
        $escapedDirectory = $Directory.Replace("'", "''")
        Write-Warning "Could not update the user PATH automatically. $($_.Exception.Message)"
        Write-Host "Run this PowerShell command to add loopcoder to PATH:"
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', '$escapedDirectory;' + [Environment]::GetEnvironmentVariable('Path', 'User'), 'User')"
        Write-Host "Then start a new PowerShell session."
    }
}

if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    Fail "install.ps1 supports Windows only; use scripts/install.sh on Unix-like systems"
}
Require-Command "cosign"

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {
}

$Os = "windows"
$Arch = Get-WindowsArch
$Tag = Resolve-LoopcoderTag -RequestedVersion $Version
$AssetVersion = $Tag -replace "^[vV]", ""
if ([string]::IsNullOrWhiteSpace($AssetVersion)) {
    Fail "version resolved to an empty asset version"
}

$ArchiveName = "loopcoder_$($AssetVersion)_$($Os)_$($Arch).zip"
$ReleaseUrl = "$GitHubBaseUrl/$Repo/releases/download/$Tag"
$CosignIdentity = if ($env:LOOPCODER_COSIGN_IDENTITY) { $env:LOOPCODER_COSIGN_IDENTITY } else { "$GitHubBaseUrl/$Repo/.github/workflows/release.yml@refs/tags/$Tag" }
$BinDir = [System.IO.Path]::GetFullPath($BinDir)
$InstallPath = Join-Path $BinDir "loopcoder.exe"

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("loopcoder-install-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    $ArchivePath = Join-Path $TempDir $ArchiveName
    $SumsPath = Join-Path $TempDir "SHA256SUMS"
    $SignaturePath = Join-Path $TempDir $ChecksumSignatureAsset
    $ExtractDir = Join-Path $TempDir "extract"
    New-Item -ItemType Directory -Path $ExtractDir -Force | Out-Null

    Write-Host "Installing loopcoder $AssetVersion for $Os/$Arch..."
    Download-File -Uri "$ReleaseUrl/SHA256SUMS" -OutFile $SumsPath -Label "SHA256SUMS"
    Download-File -Uri "$ReleaseUrl/$ChecksumSignatureAsset" -OutFile $SignaturePath -Label $ChecksumSignatureAsset
    Verify-ChecksumsSignature -SumsPath $SumsPath -SignaturePath $SignaturePath -Identity $CosignIdentity -Issuer $CosignIssuer
    Download-File -Uri "$ReleaseUrl/$ArchiveName" -OutFile $ArchivePath -Label $ArchiveName

    $expectedHash = Get-ExpectedHash -SumsPath $SumsPath -ArchiveName $ArchiveName
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $ArchivePath).Hash.ToLowerInvariant()
    if ($expectedHash -ne $actualHash) {
        Fail "checksum mismatch for ${ArchiveName}: expected $expectedHash, got $actualHash"
    }

    Expand-Archive -LiteralPath $ArchivePath -DestinationPath $ExtractDir -Force
    $sourceBinary = Get-ChildItem -LiteralPath $ExtractDir -Filter "loopcoder.exe" -Recurse -File | Select-Object -First 1
    if ($null -eq $sourceBinary) {
        Fail "$ArchiveName did not contain loopcoder.exe"
    }

    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
    $TempInstallPath = Join-Path $BinDir "loopcoder.exe.new"
    Remove-Item -LiteralPath $TempInstallPath -Force -ErrorAction SilentlyContinue
    Copy-Item -LiteralPath $sourceBinary.FullName -Destination $TempInstallPath -Force
    try {
        Move-Item -LiteralPath $TempInstallPath -Destination $InstallPath -Force
    } catch {
        Remove-Item -LiteralPath $TempInstallPath -Force -ErrorAction SilentlyContinue
        Fail "failed to install loopcoder to $InstallPath. Close any running loopcoder process and retry. $($_.Exception.Message)"
    }

    Ensure-UserPath -Directory $BinDir

    Write-Host ""
    Write-Host "Installed loopcoder $AssetVersion to $InstallPath"
    Write-Host "Run:"
    Write-Host "  loopcoder --version"
    Write-Host "  loopcoder doctor"
} finally {
    Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}
