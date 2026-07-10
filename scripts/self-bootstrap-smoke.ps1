#!/usr/bin/env pwsh
param(
    [string]$Repo = ".",
    [string]$Binary = "",
    [string]$Version = "0.7.0",
    [switch]$KeepArtifacts
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Fail([string]$Message) {
    Write-Error $Message
    exit 1
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

$repoPath = (Resolve-Path -LiteralPath $Repo).Path
if (-not (Test-Path -LiteralPath (Join-Path $repoPath ".git"))) {
    Fail "self-bootstrap smoke must run against a git checkout; .git not found under $repoPath"
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("loopcoder-self-bootstrap-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
$loopcoderHome = Join-Path $tmp "home"
$artifactDir = Join-Path $tmp "artifacts"
New-Item -ItemType Directory -Path $loopcoderHome, $artifactDir | Out-Null

$oldHome = [Environment]::GetEnvironmentVariable("LOOPCODER_HOME", "Process")
[Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $loopcoderHome, "Process")

$binaryPath = $Binary
if ([string]::IsNullOrWhiteSpace($binaryPath)) {
    $binaryPath = Join-Path $tmp ($(if ($IsWindows) { "loopcoder.exe" } else { "loopcoder" }))
    Invoke-Checked "build local loopcoder binary" {
        Push-Location -LiteralPath $repoPath
        try {
            go build -o $binaryPath ./cmd/loopcoder
        }
        finally {
            Pop-Location
        }
    }
}
else {
    $binaryPath = (Resolve-Path -LiteralPath $binaryPath).Path
}

$parentRun = "run-20260709T000000Z-wave"
$childRun = "run-20260709T000001Z-child-0-self-bootstrap-alpha"
$childRunBeta = "run-20260709T000001Z-child-1-self-bootstrap-beta"
$childRunGamma = "run-20260709T000001Z-child-2-self-bootstrap-gamma"
$runsRoot = Join-Path $repoPath ".loopcoder/runs"
$parentDir = Join-Path $runsRoot $parentRun
$childDir = Join-Path $runsRoot $childRun
$childDirs = @($childRun, $childRunBeta, $childRunGamma) | ForEach-Object { Join-Path $runsRoot $_ }

try {
    Invoke-Checked "register loopcoder checkout in machine-local project registry" {
        $script:registerOutput = @(& $binaryPath projects register --repo $repoPath --format json)
        $script:registerOutput | ForEach-Object { Write-Host $_ }
    }
    $registered = ConvertFrom-JsonOutput $registerOutput "projects register"
    if (-not $registered.project.project_id -or $registered.project.display_name -ne "loopcoder") {
        Fail "project registry did not resolve the loopcoder checkout: $($registerOutput -join "`n")"
    }

    $dbPath = Join-Path $loopcoderHome "data/loopcoder.db"
    if (-not (Test-Path -LiteralPath $dbPath)) {
        Fail "project registration did not create $dbPath"
    }
    Assert-OutsideRepo -Path $dbPath -RepoPath $repoPath -Label "v0.7.0 database"

    if (Test-Path -LiteralPath $parentDir) {
        Remove-Item -LiteralPath $parentDir -Recurse -Force
    }
    foreach ($dir in $childDirs) {
        if (Test-Path -LiteralPath $dir) {
            Remove-Item -LiteralPath $dir -Recurse -Force
        }
    }

    $planPath = Join-Path $artifactDir "child-plan.json"
    $childPlan = @{
        schema_version = "loopcoder.child_plan.v1"
        plan_id = "plan-$parentRun-self-bootstrap"
        parent_run_id = $parentRun
        root_run_id = $parentRun
        parent_depth = 0
        max_depth = 2
        max_concurrency = 2
        created_at = "2026-07-09T00:00:00Z"
        items = @(
            @{
                child_key = "self-bootstrap-alpha"
                title = "self-bootstrap-alpha"
                role = "worker"
                run_id = $childRun
                issue = 654
                scope = @{
                    repo = "."
                    paths = @("scripts/self-bootstrap-smoke.ps1")
                    issues = @(654)
                    commands = @("git status --short")
                }
                permission = "read-only"
                depends_on = @()
                aggregation = @{
                    mode = "collect"
                    required = $true
                    include_report = $true
                }
            },
            @{
                child_key = "self-bootstrap-beta"
                title = "self-bootstrap-beta"
                role = "worker"
                run_id = $childRunBeta
                issue = 655
                scope = @{
                    repo = "."
                    paths = @("docs/reference/self-bootstrap.md")
                    issues = @(655)
                    commands = @("git status --short")
                }
                permission = "read-only"
                depends_on = @()
                aggregation = @{
                    mode = "collect"
                    required = $true
                    include_report = $true
                }
            },
            @{
                child_key = "self-bootstrap-gamma"
                title = "self-bootstrap-gamma"
                role = "worker"
                run_id = $childRunGamma
                issue = 656
                scope = @{
                    repo = "."
                    paths = @("docs/reference/usage.md")
                    issues = @(656)
                    commands = @("git status --short")
                }
                permission = "read-only"
                depends_on = @("self-bootstrap-alpha", "self-bootstrap-beta")
                aggregation = @{
                    mode = "collect"
                    required = $false
                    include_report = $true
                }
            }
        )
    }
    ($childPlan | ConvertTo-Json -Depth 10) | Set-Content -LiteralPath $planPath -Encoding utf8

    Invoke-Checked "execute nested child plan with deterministic subprocess provider" {
        $script:nestedOutput = @(& $binaryPath nested run --repo $repoPath --plan $planPath --provider test-subprocess --format json)
        $script:nestedOutput | Set-Content -LiteralPath (Join-Path $artifactDir "nested-run.json") -Encoding utf8
    }
    $nested = ConvertFrom-JsonOutput $nestedOutput "nested run"
    if ($nested.status -ne "succeeded" -or @($nested.children).Count -ne 3) {
        Fail "nested run did not execute the expected three child processes: $($nestedOutput -join "`n")"
    }
    $alpha = @($nested.children | Where-Object { $_.run_id -eq $childRun }) | Select-Object -First 1
    if (-not $alpha -or $alpha.status -ne "succeeded" -or -not $alpha.attempt_path) {
        Fail "nested run did not produce a successful durable alpha child attempt"
    }

    Invoke-Checked "render status run tree" {
        $script:statusOutput = @(& $binaryPath status --repo $repoPath --run $childRun --format json)
        $script:statusOutput | Set-Content -LiteralPath (Join-Path $artifactDir "status.json") -Encoding utf8
    }
    $status = ConvertFrom-JsonOutput $statusOutput "status"
    if ($status.run_tree.root_run_id -ne $parentRun -or $status.run_tree.selected_run_id -ne $childRun -or $status.run_tree.summary.run_count -ne 4) {
        Fail "status JSON did not expose the expected parent/child run tree"
    }
    $childNode = @($status.run_tree.nodes | Where-Object { $_.run_id -eq $childRun }) | Select-Object -First 1
    if (-not $childNode -or $childNode.parent_run_id -ne $parentRun -or $childNode.issue -ne 654 -or $childNode.role -ne "worker") {
        Fail "status JSON child node is missing issue/report metadata"
    }

    Invoke-Checked "render report run tree" {
        $script:reportOutput = @(& $binaryPath report --repo $repoPath --run $childRun --format json)
        $script:reportOutput | Set-Content -LiteralPath (Join-Path $artifactDir "report.json") -Encoding utf8
    }
    $report = ConvertFrom-JsonOutput $reportOutput "report"
    if ($report.run_tree.root_run_id -ne $parentRun -or $report.run_tree.nodes.Count -ne 4) {
        Fail "report JSON did not include the parent/child run tree"
    }
    if (@($report.records | Where-Object { ($_.run_id -eq $childRun -or $_.report.work_id -eq $childRun) -and $_.report.role -eq "worker" }).Count -lt 1) {
        Fail "report JSON did not include the child worker report record"
    }

    $doctorOutput = @(& $binaryPath doctor --repo $repoPath --format json)
    $doctorOutput | Set-Content -LiteralPath (Join-Path $artifactDir "doctor.json") -Encoding utf8
    $doctorExit = $LASTEXITCODE
    $doctor = ConvertFrom-JsonOutput $doctorOutput "doctor"
    if ($doctor.runtime.database.path -ne $dbPath.Replace("\", "/") -and $doctor.runtime.database.path -ne $dbPath) {
        Fail "doctor JSON reported unexpected database path: $($doctor.runtime.database.path)"
    }
    if (-not $doctor.runtime.database.exists -or $doctor.runtime.database.status -ne "ok") {
        Fail "doctor JSON did not report healthy v0.7.0 storage"
    }
    if (-not $doctor.runtime.project_registry.registered -or $doctor.runtime.project_registry.project_id -ne $registered.project.project_id) {
        Fail "doctor JSON did not report the registered loopcoder project"
    }
    if ($doctor.runtime.nested_runs.parent_edges -lt 1 -or $doctor.runtime.nested_runs.child_edges -lt 1 -or $doctor.runtime.nested_runs.status -ne "ok") {
        Fail "doctor JSON did not report healthy nested run edges"
    }
    if (@($doctor.provider_compatibility | Where-Object { $_.provider -eq "codex" -and $_.role -eq "worker" }).Count -lt 1) {
        Fail "doctor JSON did not expose provider compatibility for the codex worker"
    }
    if ($doctorExit -ne 0) {
        Write-Host "doctor exited $doctorExit because external readiness checks may fail without gh/provider auth; runtime self-bootstrap assertions passed."
    }

    Write-Host "self-bootstrap smoke passed for v$Version"
    Write-Host "repo: $repoPath"
    Write-Host "loopcoder_home: $loopcoderHome"
    Write-Host "database: $dbPath"
    Write-Host "project_id: $($registered.project.project_id)"
    Write-Host "parent_run: $parentRun"
    Write-Host "child_run: $childRun"
    Write-Host "artifacts: $artifactDir"
}
finally {
    [Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $oldHome, "Process")
    if (-not $KeepArtifacts) {
        Remove-Item -LiteralPath $parentDir -Recurse -Force -ErrorAction SilentlyContinue
        foreach ($dir in $childDirs) {
            Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
        }
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
