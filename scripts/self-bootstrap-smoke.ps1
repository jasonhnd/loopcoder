#!/usr/bin/env pwsh
param(
    [string]$Repo = ".",
    [string]$Binary = "",
    [string]$Version = "0.8.0",
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

function Assert-DarwinArm64Host {
    if (-not $IsMacOS) {
        Fail "self-bootstrap smoke must run on darwin/arm64; current host is not macOS"
    }
    $osArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
    $processArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString().ToLowerInvariant()
    if ($osArchitecture -ne "arm64" -or $processArchitecture -ne "arm64") {
        Fail "self-bootstrap smoke must run natively on darwin/arm64; OS architecture=$osArchitecture process architecture=$processArchitecture"
    }
    return "darwin/arm64"
}

function Get-SHA256([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) {
        Fail "expected file does not exist: $Path"
    }
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Get-TreeInventory([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) {
        return @("<absent>")
    }

    $root = (Resolve-Path -LiteralPath $Path).Path
    $entries = [System.Collections.Generic.List[string]]::new()
    $entries.Add(".|directory")
    foreach ($item in @(Get-ChildItem -LiteralPath $root -Recurse -Force | Sort-Object FullName)) {
        $relative = [System.IO.Path]::GetRelativePath($root, $item.FullName).Replace("\", "/")
        if ($item.PSIsContainer) {
            $entries.Add("$relative|directory")
        }
        else {
            $entries.Add("$relative|file|$($item.Length)|$(Get-SHA256 $item.FullName)")
        }
    }
    return @($entries)
}

function Get-RepositoryRuntimeInventory([string]$RepoPath) {
    $entries = [System.Collections.Generic.List[string]]::new()
    foreach ($relativeRoot in @(".loopcoder/runs", ".loopcoder/logs", ".loopcoder/recovery", ".loopcoder/relay")) {
        foreach ($entry in @(Get-TreeInventory (Join-Path $RepoPath $relativeRoot))) {
            $entries.Add("$relativeRoot|$entry")
        }
    }
    return @($entries)
}

function Assert-InventoryUnchanged([string[]]$Before, [string[]]$After, [string]$Label) {
    $difference = @(Compare-Object -ReferenceObject $Before -DifferenceObject $After)
    if ($difference.Count -gt 0) {
        $rendered = $difference | ForEach-Object { "$($_.SideIndicator) $($_.InputObject)" }
        Fail "$Label changed unexpectedly:`n$($rendered -join "`n")"
    }
}

# The platform refusal must happen before temporary state, storage, provider, or
# repository mutation. Keep this call above repo resolution and temp creation.
$hostTuple = Assert-DarwinArm64Host

$repoPath = (Resolve-Path -LiteralPath $Repo).Path
if (-not (Test-Path -LiteralPath (Join-Path $repoPath ".git"))) {
    Fail "self-bootstrap smoke must run against a git checkout; .git not found under $repoPath"
}
$repoRuntimeBefore = @(Get-RepositoryRuntimeInventory $repoPath)

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("loopcoder-self-bootstrap-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
$loopcoderHome = Join-Path $tmp "home"
$artifactDir = Join-Path $tmp "artifacts"
New-Item -ItemType Directory -Path $loopcoderHome, $artifactDir | Out-Null

$oldHome = [Environment]::GetEnvironmentVariable("LOOPCODER_HOME", "Process")
[Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $loopcoderHome, "Process")

$usingStagedBinary = -not [string]::IsNullOrWhiteSpace($Binary)
$binaryPath = $Binary
if ([string]::IsNullOrWhiteSpace($binaryPath)) {
    $binaryPath = Join-Path $tmp "loopcoder"
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

$binaryHash = Get-SHA256 $binaryPath
Invoke-Checked "record selected loopcoder binary identity" {
    $script:versionOutput = @(& $binaryPath version)
    $script:versionOutput | ForEach-Object { Write-Host $_ }
}
$versionText = ($versionOutput -join "`n").Trim()
$versionText | Set-Content -LiteralPath (Join-Path $artifactDir "candidate-version.txt") -Encoding utf8
$binaryHash | Set-Content -LiteralPath (Join-Path $artifactDir "candidate-sha256.txt") -Encoding utf8
if ($versionText -notmatch "(^|\s)platform=darwin/arm64(\s|$)") {
    Fail "selected binary is not a darwin/arm64 binary: $versionText"
}
$plainVersion = $Version.TrimStart("v")
if ($usingStagedBinary) {
    $versionPattern = "(^|\s)version=v?$([regex]::Escape($plainVersion))(\s|$)"
    if ($versionText -notmatch $versionPattern) {
        Fail "staged binary did not report requested version $plainVersion`: $versionText"
    }
    if ($versionText -match "(^|\s)(commit|date)=unknown(\s|$)") {
        Fail "staged binary must report non-placeholder commit and date: $versionText"
    }
}

$parentRun = "run-20260709T000000Z-wave"
$childRun = "run-20260709T000001Z-child-0-self-bootstrap-alpha"
$childRunBeta = "run-20260709T000001Z-child-1-self-bootstrap-beta"
$childRunGamma = "run-20260709T000001Z-child-2-self-bootstrap-gamma"
$mutationParentRun = "run-20260709T000002Z-wave-read-only-mutation"
$mutationChildRun = "run-20260709T000003Z-child-0-read-only-mutation"
$writeParentRun = "run-20260709T000004Z-wave-bounded-write"
$writeChildRun = "run-20260709T000005Z-child-0-bounded-write"
$boundedWriteWorktree = ""

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
    Assert-OutsideRepo -Path $dbPath -RepoPath $repoPath -Label "v0.8.0 database"

    $databaseHashBeforePlan = Get-SHA256 $dbPath
    $backupRoot = Join-Path $loopcoderHome "data/backups"
    $backupInventoryBeforePlan = @(Get-TreeInventory $backupRoot)
    Invoke-Checked "render read-only fresh-schema storage migration plan" {
        $script:storagePlanOutput = @(& $binaryPath migrate storage --format json)
        $script:storagePlanOutput | Set-Content -LiteralPath (Join-Path $artifactDir "storage-plan.json") -Encoding utf8
        $script:storagePlanOutput | ForEach-Object { Write-Host $_ }
    }
    $storagePlan = ConvertFrom-JsonOutput $storagePlanOutput "migrate storage"
    if (-not $storagePlan.dry_run -or $storagePlan.applied -or $storagePlan.status -ne "planned") {
        Fail "fresh-schema migration plan was not read-only"
    }
    if ($storagePlan.plan.source_schema_version -ne 30 -or $storagePlan.plan.target_schema_version -ne 30 -or $storagePlan.plan.status -ne "current") {
        Fail "fresh-schema migration plan did not report schema 30 as current"
    }
    if ($storagePlan.plan.backup_required) {
        Fail "fresh-schema migration plan unexpectedly required a backup"
    }
    if ((Get-SHA256 $dbPath) -ne $databaseHashBeforePlan) {
        Fail "read-only fresh-schema migration plan modified the database"
    }
    Assert-InventoryUnchanged -Before $backupInventoryBeforePlan -After @(Get-TreeInventory $backupRoot) -Label "fresh-schema backup inventory"

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
    Assert-OutsideRepo -Path $alpha.attempt_path -RepoPath $repoPath -Label "nested child attempt"

    $mutationMarkerName = ".loopcoder-read-only-mutation-fixture"
    $mutationMarkerPath = Join-Path $repoPath $mutationMarkerName
    $mutationPlanPath = Join-Path $artifactDir "read-only-mutation-plan.json"
    $mutationPlan = @{
        schema_version = "loopcoder.child_plan.v1"
        plan_id = "plan-$mutationParentRun"
        parent_run_id = $mutationParentRun
        root_run_id = $mutationParentRun
        parent_depth = 0
        max_depth = 2
        max_concurrency = 1
        created_at = "2026-07-09T00:00:02Z"
        items = @(
            @{
                child_key = "read-only-mutation"
                title = "read-only-mutation"
                role = "worker"
                run_id = $mutationChildRun
                issue = 1006
                scope = @{
                    repo = "."
                    paths = @("scripts/self-bootstrap-smoke.ps1")
                    issues = @(1006)
                    commands = @("printf mutation > $mutationMarkerName")
                }
                permission = "read-only"
                depends_on = @()
                aggregation = @{
                    mode = "collect"
                    required = $true
                    include_report = $true
                }
            }
        )
    }
    ($mutationPlan | ConvertTo-Json -Depth 10) | Set-Content -LiteralPath $mutationPlanPath -Encoding utf8

    Write-Host "==> verify read-only mutation fixture fails closed"
    $mutationOutput = @(& $binaryPath nested run --repo $repoPath --plan $mutationPlanPath --provider test-subprocess --format json)
    $mutationExitCode = $LASTEXITCODE
    $mutationOutput | Set-Content -LiteralPath (Join-Path $artifactDir "read-only-mutation-result.json") -Encoding utf8
    if ($mutationExitCode -eq 0) {
        Fail "read-only mutation fixture unexpectedly succeeded"
    }
    $mutationResult = ConvertFrom-JsonOutput $mutationOutput "read-only mutation fixture"
    $mutationChild = @($mutationResult.children) | Select-Object -First 1
    if ($mutationResult.status -ne "needs-human" -or -not $mutationChild -or $mutationChild.status -ne "needs-human" -or $mutationChild.outcome -ne "read_only_policy_violation") {
        Fail "read-only mutation fixture did not produce the typed needs-human policy outcome"
    }
    if (-not $mutationChild.read_only_enforcement -or $mutationChild.read_only_enforcement.verification -ne "policy-violation") {
        Fail "read-only mutation fixture omitted policy-violation enforcement evidence"
    }
    if (@($mutationChild.read_only_enforcement.violations | Where-Object { $_.code -eq "untracked_file_created" }).Count -lt 1) {
        Fail "read-only mutation fixture omitted the untracked-file violation code"
    }
    if (-not (Test-Path -LiteralPath $mutationMarkerPath)) {
        Fail "read-only executor remediated the mutation instead of preserving evidence"
    }
    Remove-Item -LiteralPath $mutationMarkerPath -Force

    $writeTargetRelative = "docs/reference/self-bootstrap.md"
    $writeTargetPath = Join-Path $repoPath $writeTargetRelative
    $writeTargetHashBefore = Get-SHA256 $writeTargetPath
    $worktreeCountBeforeWrite = @(& git -C $repoPath worktree list --porcelain | Where-Object { $_ -match '^worktree ' }).Count
    if ($LASTEXITCODE -ne 0) {
        Fail "could not inventory worktrees before bounded-write smoke"
    }
    $writePlanPath = Join-Path $artifactDir "bounded-write-plan.json"
    $writePlan = @{
        schema_version = "loopcoder.child_plan.v1"
        plan_id = "plan-$writeParentRun"
        parent_run_id = $writeParentRun
        root_run_id = $writeParentRun
        parent_depth = 0
        max_depth = 2
        max_concurrency = 1
        created_at = "2026-07-09T00:00:04Z"
        items = @(
            @{
                child_key = "bounded-write"
                title = "bounded-write"
                role = "worker"
                run_id = $writeChildRun
                issue = 1007
                scope = @{
                    repo = "."
                    paths = @($writeTargetRelative)
                    issues = @(1007)
                    commands = @("printf 'bounded write smoke\n' >> $writeTargetRelative")
                }
                permission = "write"
                depends_on = @()
                aggregation = @{
                    mode = "collect"
                    required = $true
                    include_report = $true
                }
            }
        )
    }
    ($writePlan | ConvertTo-Json -Depth 10) | Set-Content -LiteralPath $writePlanPath -Encoding utf8

    Invoke-Checked "execute packaged bounded-write child smoke" {
        $script:writeOutput = @(& $binaryPath nested run --repo $repoPath --plan $writePlanPath --provider test-subprocess --base-branch main --format json)
        $script:writeOutput | Set-Content -LiteralPath (Join-Path $artifactDir "bounded-write-result.json") -Encoding utf8
    }
    $writeResult = ConvertFrom-JsonOutput $writeOutput "bounded-write nested run"
    $writeChild = @($writeResult.children) | Select-Object -First 1
    if ($writeResult.status -ne "succeeded" -or -not $writeChild -or $writeChild.status -ne "succeeded") {
        Fail "bounded-write packaged smoke did not succeed"
    }
    if (-not $writeChild.mutation_manifest -or $writeChild.mutation_manifest.verification -ne "passed" -or @($writeChild.mutation_manifest.changes | Where-Object { $_.path -eq $writeTargetRelative }).Count -lt 1) {
        Fail "bounded-write packaged smoke omitted the allowed mutation manifest"
    }
    if (-not $writeChild.worktree_path -or -not $writeChild.attempt_path) {
        Fail "bounded-write packaged smoke omitted its preserved worktree or attempt path"
    }
    Assert-OutsideRepo -Path $writeChild.worktree_path -RepoPath $repoPath -Label "bounded-write child worktree"
    Assert-OutsideRepo -Path $writeChild.attempt_path -RepoPath $repoPath -Label "bounded-write child attempt"
    $boundedWriteWorktree = $writeChild.worktree_path
    $worktreeCountAfterWrite = @(& git -C $repoPath worktree list --porcelain | Where-Object { $_ -match '^worktree ' }).Count
    if ($LASTEXITCODE -ne 0 -or $worktreeCountAfterWrite -ne ($worktreeCountBeforeWrite + 1)) {
        Fail "bounded-write packaged smoke did not register exactly one isolated worktree"
    }
    if ((Get-SHA256 $writeTargetPath) -ne $writeTargetHashBefore) {
        Fail "bounded-write packaged smoke modified the parent checkout"
    }
    $writeChildTarget = Join-Path $writeChild.worktree_path $writeTargetRelative
    if (-not (Test-Path -LiteralPath $writeChildTarget) -or (Get-SHA256 $writeChildTarget) -eq $writeTargetHashBefore) {
        Fail "bounded-write packaged smoke did not preserve the child mutation"
    }
    Invoke-Checked "remove packaged bounded-write smoke worktree" {
        & git -C $repoPath worktree remove --force $writeChild.worktree_path
    }
    $boundedWriteWorktree = ""
    $worktreeCountAfterCleanup = @(& git -C $repoPath worktree list --porcelain | Where-Object { $_ -match '^worktree ' }).Count
    if ($LASTEXITCODE -ne 0 -or $worktreeCountAfterCleanup -ne $worktreeCountBeforeWrite) {
        Fail "bounded-write packaged smoke cleanup left a registered worktree"
    }

    Invoke-Checked "render status run tree (human)" {
        $script:statusTextOutput = @(& $binaryPath status --repo $repoPath --run $childRun --format text)
        $script:statusTextOutput | Set-Content -LiteralPath (Join-Path $artifactDir "status.txt") -Encoding utf8
        $script:statusTextOutput | ForEach-Object { Write-Host $_ }
    }
    if (($statusTextOutput -join "`n") -notmatch [regex]::Escape($childRun)) {
        Fail "status human output did not include selected child run $childRun"
    }

    Invoke-Checked "render status run tree (JSON)" {
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

    Invoke-Checked "render report run tree (human)" {
        $script:reportTextOutput = @(& $binaryPath report --repo $repoPath --run $childRun --format text)
        $script:reportTextOutput | Set-Content -LiteralPath (Join-Path $artifactDir "report.txt") -Encoding utf8
        $script:reportTextOutput | ForEach-Object { Write-Host $_ }
    }
    if (($reportTextOutput -join "`n") -notmatch [regex]::Escape($childRun)) {
        Fail "report human output did not include selected child run $childRun"
    }

    Invoke-Checked "render report run tree (JSON)" {
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
        Fail "doctor JSON did not report healthy v0.8.0 storage"
    }
    if (-not $doctor.runtime.project_registry.registered -or $doctor.runtime.project_registry.project_id -ne $registered.project.project_id) {
        Fail "doctor JSON did not report the registered loopcoder project"
    }
    foreach ($runtimePath in @(
        $doctor.runtime.project_registry.payload_root,
        $doctor.runtime.project_registry.runs_root,
        $doctor.runtime.project_registry.relay_root,
        $doctor.runtime.project_registry.recovery_root,
        $doctor.runtime.project_registry.audit_root,
        $doctor.runtime.project_registry.logs_root,
        $doctor.runtime.project_registry.tmp_root
    )) {
        if ([string]::IsNullOrWhiteSpace($runtimePath)) {
            Fail "doctor JSON omitted a registered runtime payload path"
        }
        Assert-OutsideRepo -Path $runtimePath -RepoPath $repoPath -Label "doctor runtime payload path"
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

    Assert-InventoryUnchanged -Before $repoRuntimeBefore -After @(Get-RepositoryRuntimeInventory $repoPath) -Label "repository-local runtime payload inventory"

    $evidence = [ordered]@{
        schema_version = "loopcoder.self_bootstrap_evidence.v1"
        requested_version = $plainVersion
        staged_candidate = $usingStagedBinary
        host_tuple = $hostTuple
        binary = [ordered]@{
            path = $binaryPath
            sha256 = $binaryHash
            version_output = $versionText
        }
        provider = "test-subprocess"
        paid_provider_calls = 0
        project_id = $registered.project.project_id
        database_path = $dbPath
        database_outside_repo = $true
        registered_payload_root = $doctor.runtime.project_registry.payload_root
        registered_payload_outside_repo = $true
        repository_runtime_unchanged = $true
        storage_plan = [ordered]@{
            path = (Join-Path $artifactDir "storage-plan.json")
            source_schema_version = $storagePlan.plan.source_schema_version
            target_schema_version = $storagePlan.plan.target_schema_version
            status = $storagePlan.plan.status
            database_unchanged = $true
            backup_created = $false
        }
        runs = [ordered]@{
            parent = $parentRun
            children = @($childRun, $childRunBeta, $childRunGamma)
            status = $nested.status
            mutation_fixture_child = $mutationChildRun
            mutation_fixture_status = $mutationChild.status
            bounded_write_child = $writeChildRun
            bounded_write_status = $writeChild.status
            bounded_write_manifest = $writeChild.mutation_manifest.manifest_fingerprint
        }
        artifacts = [ordered]@{
            status_human = (Join-Path $artifactDir "status.txt")
            status_json = (Join-Path $artifactDir "status.json")
            report_human = (Join-Path $artifactDir "report.txt")
            report_json = (Join-Path $artifactDir "report.json")
            doctor_json = (Join-Path $artifactDir "doctor.json")
            bounded_write_json = (Join-Path $artifactDir "bounded-write-result.json")
        }
    }
    $evidenceJson = $evidence | ConvertTo-Json -Depth 8
    $evidenceJson | Set-Content -LiteralPath (Join-Path $artifactDir "self-bootstrap-evidence.json") -Encoding utf8

    $humanEvidence = @(
        "self-bootstrap smoke passed for v$plainVersion",
        "host: $hostTuple",
        "candidate: $binaryPath",
        "candidate_sha256: $binaryHash",
        "provider: test-subprocess (paid calls: 0)",
        "repo: $repoPath",
        "loopcoder_home: $loopcoderHome",
        "database: $dbPath",
        "project_id: $($registered.project.project_id)",
        "parent_run: $parentRun",
        "child_runs: $childRun, $childRunBeta, $childRunGamma",
        "bounded_write_child: $writeChildRun",
        "bounded_write_manifest: $($writeChild.mutation_manifest.manifest_fingerprint)",
        "bounded_write_parent_unchanged: true",
        "repository_runtime_unchanged: true",
        "artifacts: $artifactDir"
    )
    $humanEvidence | Set-Content -LiteralPath (Join-Path $artifactDir "self-bootstrap-evidence.txt") -Encoding utf8
    $humanEvidence | ForEach-Object { Write-Host $_ }
    Write-Host "self-bootstrap evidence JSON:"
    $evidenceJson | Write-Host

    if ($KeepArtifacts) {
        Write-Host "retained artifacts: $artifactDir"
    }
}
finally {
    if (-not [string]::IsNullOrWhiteSpace($boundedWriteWorktree)) {
        & git -C $repoPath worktree remove --force $boundedWriteWorktree 2>$null
    }
    [Environment]::SetEnvironmentVariable("LOOPCODER_HOME", $oldHome, "Process")
    if (-not $KeepArtifacts) {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
