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
        go build -o $binaryPath ./cmd/loopcoder
    }
}
else {
    $binaryPath = (Resolve-Path -LiteralPath $binaryPath).Path
}

$parentRun = "run-20260709T000000Z-wave"
$childRun = "run-20260709T000001Z-child-0-self-bootstrap"
$runsRoot = Join-Path $repoPath ".loopcoder/runs"
$parentDir = Join-Path $runsRoot $parentRun
$childDir = Join-Path $runsRoot $childRun
$workerDir = Join-Path $childDir "workers"

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
    if (Test-Path -LiteralPath $childDir) {
        Remove-Item -LiteralPath $childDir -Recurse -Force
    }
    New-Item -ItemType Directory -Path $parentDir, $workerDir | Out-Null

    @(
        @{
            version = 1
            ts = "2026-07-09T00:00:00Z"
            run_id = $parentRun
            state = "planned"
            child_run_id = $childRun
            source = "self-bootstrap-smoke"
        },
        @{
            version = 1
            ts = "2026-07-09T00:00:01Z"
            run_id = $parentRun
            previous_state = "planned"
            state = "running"
            child_run_id = $childRun
            source = "self-bootstrap-smoke"
        }
    ) | ForEach-Object {
        ($_ | ConvertTo-Json -Compress) | Add-Content -LiteralPath (Join-Path $parentDir "lifecycle.jsonl") -Encoding utf8
    }

    @(
        @{
            version = 1
            ts = "2026-07-09T00:00:02Z"
            run_id = $childRun
            parent_run_id = $parentRun
            state = "planned"
            source = "self-bootstrap-smoke"
        },
        @{
            version = 1
            ts = "2026-07-09T00:00:03Z"
            run_id = $childRun
            parent_run_id = $parentRun
            previous_state = "planned"
            state = "running"
            source = "self-bootstrap-smoke"
        },
        @{
            version = 1
            ts = "2026-07-09T00:00:04Z"
            run_id = $childRun
            parent_run_id = $parentRun
            previous_state = "running"
            state = "succeeded"
            source = "self-bootstrap-smoke"
        }
    ) | ForEach-Object {
        ($_ | ConvertTo-Json -Compress) | Add-Content -LiteralPath (Join-Path $childDir "lifecycle.jsonl") -Encoding utf8
    }

    $attempt = @{
        version = 1
        job_id = "job-654-1"
        issue = 654
        attempt = 1
        provider = "codex"
        pid = 0
        phase = "self_bootstrap_fixture"
        status = "succeeded"
        branch = "loop/issue-654"
        started_at = "2026-07-09T00:00:02Z"
        heartbeat_at = "2026-07-09T00:00:04Z"
        last_progress_at = "2026-07-09T00:00:04Z"
        log_bytes = 0
        exit_code = 0
        report = @{
            role = "worker"
            provider = "codex"
            model = "gpt-5.5"
            model_source = "parsed"
            effort = "high"
            permission = "write"
            action = "self-bootstrap fixture for issue #654"
            exit_code = 0
            started_at = "2026-07-09T00:00:02Z"
            ended_at = "2026-07-09T00:00:04Z"
            duration_ms = 2000
            usage = @{
                total_tokens = 6540
            }
            verified = $true
            work_id = $childRun
            issue = 654
            branch = "loop/issue-654"
        }
    }
    ($attempt | ConvertTo-Json -Depth 8) | Set-Content -LiteralPath (Join-Path $workerDir "job-654-1.attempt.json") -Encoding utf8

    Invoke-Checked "render status run tree" {
        $script:statusOutput = @(& $binaryPath status --repo $repoPath --run $childRun --format json)
        $script:statusOutput | Set-Content -LiteralPath (Join-Path $artifactDir "status.json") -Encoding utf8
    }
    $status = ConvertFrom-JsonOutput $statusOutput "status"
    if ($status.run_tree.root_run_id -ne $parentRun -or $status.run_tree.selected_run_id -ne $childRun -or $status.run_tree.summary.run_count -ne 2) {
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
    if ($report.run_tree.root_run_id -ne $parentRun -or $report.run_tree.nodes.Count -ne 2) {
        Fail "report JSON did not include the parent/child run tree"
    }
    if (@($report.records | Where-Object { $_.run_id -eq $childRun -and $_.report.role -eq "worker" }).Count -lt 1) {
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
        Remove-Item -LiteralPath $childDir -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
