#Requires -Version 7
<#
.SYNOPSIS
  loopcoder Worker adapter (M1) — provider: codex.

  Takes one GitHub issue and runs the mechanical worker step:
    fresh git worktree  ->  codex implements (headless)  ->  commit  ->  push  ->  open PR

  Design notes:
  - codex ONLY edits files. This script does commit / push / PR (deterministic, VCS stays in our hands).
  - codex runs headless. Prompt is fed via stdin from a file (`- < promptfile`) which also closes stdin,
    so codex does NOT hang waiting for input (the failure we hit before).
  - --dangerously-bypass-approvals-and-sandbox: codex runs unattended with no prompts. The worktree is the
    blast radius; do not point this at anything you would not let an agent edit autonomously.

.EXAMPLE
  pwsh scripts/dispatch-worker.ps1 -Repo . -IssueNumber 1 -IssueTitle "Add README" -IssueBody "..."
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Repo,
  [Parameter(Mandatory)][int]$IssueNumber,
  [Parameter(Mandatory)][string]$IssueTitle,
  [string]$IssueBody = '',
  [string]$BaseBranch = 'main',
  [string]$Branch,
  [string]$RunId,
  [int]$Attempt = 1,
  [string]$RecoveryContext = '',
  [ValidateSet('codex')][string]$Provider = 'codex',   # v1: codex only (Worker port is provider-pluggable)
  [string]$Model,
  [string]$Effort,
  [switch]$KeepWorktree
)
$ErrorActionPreference = 'Stop'
function Log($m){ Write-Host "[loopcoder] $m" }
function Quote-CmdArg($arg){ '"' + ($arg -replace '"','\"') + '"' }
function Get-UtcIso(){ (Get-Date).ToUniversalTime().ToString('o') }
function Get-UtcCompact(){ (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') }
function Scrub-RecoveryText {
  param([AllowNull()][string]$Text)
  if ($null -eq $Text) { return '' }

  $scrubbed = $Text
  $scrubbed = $scrubbed -replace '(?i)(ghp|github_pat|gho|ghu|ghs|ghr)_[A-Za-z0-9_]+', '[REDACTED_GITHUB_TOKEN]'
  $scrubbed = $scrubbed -replace '(?i)sk-[A-Za-z0-9_-]{20,}', '[REDACTED_API_KEY]'
  $scrubbed = $scrubbed -replace '(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+', '$1[REDACTED_TOKEN]'
  $scrubbed = $scrubbed -replace '(?i)((token|password|secret|api[_-]?key)\s*[=:]\s*)\S+', '$1[REDACTED_SECRET]'
  return $scrubbed
}
function Get-AttemptLogBytes(){
  try {
    if ($script:logFile -and (Test-Path -LiteralPath $script:logFile)) {
      return [int64](Get-Item -LiteralPath $script:logFile).Length
    }
  } catch {}
  return [int64]0
}
function Write-RecoveryBrief {
  param(
    [string]$FailurePhase,
    [AllowNull()][string]$ErrorMessage
  )
  try {
    if (-not $script:runPath -or -not $script:jobId) { return }

    $recoveryPath = Join-Path $script:runPath 'recovery'
    New-Item -ItemType Directory -Force -Path $recoveryPath | Out-Null
    $briefPath = Join-Path $recoveryPath "$($script:jobId)-context.md"

    $changedFiles = '(worktree path does not exist)'
    if ($script:wt -and (Test-Path -LiteralPath $script:wt -PathType Container)) {
      try {
        $statusLines = @(git -C $script:wt status --porcelain 2>$null)
        if ($LASTEXITCODE -eq 0) {
          $changedFiles = if ($statusLines.Count -gt 0) { $statusLines -join [Environment]::NewLine } else { '(none)' }
        } else {
          $changedFiles = "(git status failed with exit $LASTEXITCODE)"
        }
      } catch {
        $changedFiles = "(git status failed: $($_.Exception.Message))"
      }
    }

    $logTail = '(log file not found)'
    if ($script:logFile -and (Test-Path -LiteralPath $script:logFile)) {
      try {
        $tailLines = @(Get-Content -LiteralPath $script:logFile -Tail 50 -ErrorAction Stop)
        $logTail = Scrub-RecoveryText ($tailLines -join [Environment]::NewLine)
        if ([string]::IsNullOrWhiteSpace($logTail)) { $logTail = '(log tail empty)' }
      } catch {
        $logTail = "(failed to read log tail: $($_.Exception.Message))"
      }
    }

    $prStatus = 'PR lookup failed or unavailable'
    try {
      Push-Location $Repo
      try {
        $prJson = & gh pr list --head $Branch --json number,url 2>$null
        if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($prJson)) {
          $prs = @($prJson | ConvertFrom-Json)
          if ($prs.Count -gt 0) {
            $prStatus = ($prs | ForEach-Object { "#$($_.number) $($_.url)" }) -join [Environment]::NewLine
          } else {
            $prStatus = 'No open PR found for branch'
          }
        } else {
          $prStatus = "gh pr list failed with exit $LASTEXITCODE"
        }
      } finally {
        Pop-Location
      }
    } catch {
      $prStatus = "PR lookup failed: $($_.Exception.Message)"
    }

    $safeError = Scrub-RecoveryText $ErrorMessage
    $brief = @"
# Recovery context for issue #$IssueNumber

- Issue: #$IssueNumber
- Title: $IssueTitle
- Branch: $Branch
- Worktree path: $script:wt
- Log path: $script:logFile
- Summary path: $script:summaryFile
- Attempt: $script:attempt
- Last phase: $FailurePhase
- Status: $script:attemptStatus
- Error: $safeError

## Changed files

``````text
$changedFiles
``````

## Existing PR for branch

``````text
$prStatus
``````

## Scrubbed log tail (last 50 lines)

``````text
$logTail
``````
"@
    Set-Content -LiteralPath $briefPath -Value $brief -Encoding utf8
  } catch {
    Write-Warning "[loopcoder] failed to write recovery brief for $($script:jobId): $($_.Exception.Message)"
  }
}
function Write-AttemptSidecar {
  param(
    [string]$Phase,
    [string]$Status,
    [object]$ExitCode,
    [AllowNull()][string]$ErrorMessage
  )
  try {
    if (-not $script:attemptPath) { return }
    $now = Get-UtcIso
    $currentLogBytes = Get-AttemptLogBytes
    $phaseAdvanced = $false
    if ($Phase -and $Phase -ne $script:attemptPhase) {
      $script:attemptPhase = $Phase
      $phaseAdvanced = $true
    }
    $logAdvanced = $currentLogBytes -gt $script:attemptLogBytes
    if ($phaseAdvanced -or $logAdvanced -or -not $script:attemptLastProgressAt) {
      $script:attemptLastProgressAt = $now
    }
    $script:attemptHeartbeatAt = $now
    $script:attemptLogBytes = $currentLogBytes
    $statusChanged = $false
    if ($Status) {
      $statusChanged = $Status -ne $script:attemptStatus
      $script:attemptStatus = $Status
    }
    $exitCodeChanged = $false
    if ($PSBoundParameters.ContainsKey('ExitCode')) {
      $exitCodeChanged = $ExitCode -ne $script:attemptExitCode
      $script:attemptExitCode = $ExitCode
    }
    $errorChanged = $false
    if ($PSBoundParameters.ContainsKey('ErrorMessage')) {
      $errorChanged = $ErrorMessage -ne $script:attemptError
      $script:attemptError = $ErrorMessage
    }

    New-Item -ItemType Directory -Force -Path $script:runPath | Out-Null
    New-Item -ItemType Directory -Force -Path $script:workersPath | Out-Null

    $attemptRecord = [ordered]@{
      version = 1
      job_id = $script:jobId
      issue = $IssueNumber
      attempt = $script:attempt
      provider = $Provider
      pid = $PID
      phase = $script:attemptPhase
      status = $script:attemptStatus
      started_at = $script:attemptStartedAt
      heartbeat_at = $script:attemptHeartbeatAt
      last_progress_at = $script:attemptLastProgressAt
      log_bytes = $script:attemptLogBytes
      exit_code = $script:attemptExitCode
      error = $script:attemptError
    }
    $attemptRecord | ConvertTo-Json -Compress | Set-Content -LiteralPath $script:attemptPath -Encoding utf8

    if ($phaseAdvanced -or $statusChanged -or $exitCodeChanged -or $errorChanged) {
      [ordered]@{
        ts = $now
        run_id = $RunId
        job_id = $script:jobId
        issue = $IssueNumber
        phase = $script:attemptPhase
        status = $script:attemptStatus
        log_bytes = $script:attemptLogBytes
        exit_code = $script:attemptExitCode
        error = $script:attemptError
      } | ConvertTo-Json -Compress | Add-Content -LiteralPath $script:eventsPath -Encoding utf8
    }
  } catch {
    Write-Warning "[loopcoder] failed to write durable attempt state $($script:attemptPath): $($_.Exception.Message)"
  }
}

$Repo = (Resolve-Path -LiteralPath $Repo).Path
if (-not $Branch) { $Branch = "loop/issue-$IssueNumber" }
if ([string]::IsNullOrWhiteSpace($RunId)) { $RunId = "run-$(Get-UtcCompact)-issue-$IssueNumber" }

Push-Location $Repo
try { $slug = (& gh repo view --json nameWithOwner -q .nameWithOwner) } finally { Pop-Location }
if (-not $slug) { throw "could not resolve GitHub repo (gh repo view) — need a repo with a GitHub remote." }

$scratch     = Join-Path ([IO.Path]::GetTempPath()) ("loopcoder-" + [guid]::NewGuid().ToString('N').Substring(0,8))
New-Item -ItemType Directory -Force -Path $scratch | Out-Null
$wt          = Join-Path $scratch 'wt'
$promptFile  = Join-Path $scratch 'prompt.txt'
$summaryFile = Join-Path $scratch 'summary.txt'
$logFile     = Join-Path $scratch 'codex.log'
$attempt     = $Attempt
$jobId       = "job-$IssueNumber-$PID"
$runPath     = Join-Path (Join-Path (Join-Path $Repo '.loopcoder') 'runs') $RunId
$workersPath = Join-Path $runPath 'workers'
$eventsPath  = Join-Path $runPath 'events.jsonl'
$attemptPath = Join-Path $workersPath "$jobId.attempt.json"
$attemptStartedAt = Get-UtcIso
$attemptHeartbeatAt = $attemptStartedAt
$attemptLastProgressAt = $attemptStartedAt
$attemptPhase = $null
$attemptStatus = 'running'
$attemptLogBytes = [int64]0
$attemptExitCode = $null
$attemptError = $null
$activePhase = 'worktree_created'
$preserveArtifacts = $false
$dispatchSucceeded = $false

try {
  Log "issue #$IssueNumber  ->  branch $Branch   (repo $slug, provider $Provider)"
  git -C $Repo fetch -q origin $BaseBranch
  $repoKey = [Convert]::ToHexString([System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($Repo.ToLowerInvariant())))
  $worktreeMutex = [System.Threading.Mutex]::new($false, "Global\loopcoder-worktree-add-$repoKey")
  $worktreeLockAcquired = $false
  $worktreeAddExitCode = $null
  try {
    $worktreeLockAcquired = $worktreeMutex.WaitOne([TimeSpan]::FromSeconds(60))
    if (-not $worktreeLockAcquired) { throw "timed out after 60 seconds waiting for git worktree add lock for repo $Repo" }
    git -C $Repo worktree add -b $Branch $wt "origin/$BaseBranch" 2>&1 | ForEach-Object { Log $_ }
    $worktreeAddExitCode = $LASTEXITCODE
  }
  finally {
    if ($worktreeLockAcquired) { $worktreeMutex.ReleaseMutex() }
    $worktreeMutex.Dispose()
  }
  if ($worktreeAddExitCode -ne 0) { throw "git worktree add failed" }
  Write-AttemptSidecar -Phase $activePhase -Status 'running'

  $activePhase = 'prompt_written'
  $prompt = @"
You are implementing GitHub issue #$IssueNumber. The current working directory is a fresh git worktree on branch $Branch.

# Title
$IssueTitle

# Details
$IssueBody

# Rules
- Implement the change so the issue is satisfied. Keep it minimal and follow existing conventions in the repo.
- You may read files and run commands, but do NOT run git commit or git push — the harness commits and opens the PR.
- When finished, print a 2-4 sentence summary of exactly what you changed.
"@
  if (-not [string]::IsNullOrWhiteSpace($RecoveryContext)) {
    $prompt = $prompt + @"

## Recovery context from a prior failed attempt (reuse what is valid, fix what failed)

$RecoveryContext
"@
  }
  Set-Content -LiteralPath $promptFile -Value $prompt -Encoding utf8
  Write-AttemptSidecar -Phase $activePhase -Status 'running'

  $activePhase = 'codex_started'
  Log "codex implementing (headless, stdin-from-file)..."
  $codexArgs = @(
    'exec',
    '--cd', $wt,
    '--dangerously-bypass-approvals-and-sandbox',
    '--skip-git-repo-check'
  )
  if (-not [string]::IsNullOrWhiteSpace($Model)) { $codexArgs += @('-m', $Model) }
  if (-not [string]::IsNullOrWhiteSpace($Effort)) { $codexArgs += @('-c', "model_reasoning_effort=$Effort") }
  $codexArgs += @('-o', $summaryFile, '-')
  $codexCommand = 'codex ' + (($codexArgs | ForEach-Object { Quote-CmdArg $_ }) -join ' ')
  Write-AttemptSidecar -Phase $activePhase -Status 'running'
  cmd /c "$codexCommand < `"$promptFile`" > `"$logFile`" 2>&1"
  $codexExitCode = $LASTEXITCODE
  $activePhase = 'codex_exited'
  Write-AttemptSidecar -Phase $activePhase -Status 'running' -ExitCode $codexExitCode
  if ($codexExitCode -ne 0) { throw "codex exec failed (exit $codexExitCode). See $logFile" }
  $summary = if (Test-Path $summaryFile) { (Get-Content -LiteralPath $summaryFile -Raw).Trim() } else { '(codex produced no summary)' }

  $activePhase = 'dirty_checked'
  $dirty = git -C $wt status --porcelain
  if (-not $dirty) { throw "codex made no file changes for issue #$IssueNumber (nothing to commit)" }
  Write-AttemptSidecar -Phase $activePhase -Status 'running'

  Log "commit + push"
  git -C $wt add -A
  $activePhase = 'committed'
  git -C $wt commit -q -m "$IssueTitle (closes #$IssueNumber)"
  if ($LASTEXITCODE -ne 0) { throw "git commit failed" }
  Write-AttemptSidecar -Phase $activePhase -Status 'running'
  $activePhase = 'pushed'
  git -C $wt push -q -u origin $Branch
  if ($LASTEXITCODE -ne 0) { throw "git push failed" }
  Write-AttemptSidecar -Phase $activePhase -Status 'running'

  Log "open PR"
  $activePhase = 'pr_opened'
  $body  = "Closes #$IssueNumber`n`n$summary`n`n— opened by loopcoder (worker: $Provider)"
  $prUrl = & gh pr create -R $slug --head $Branch --base $BaseBranch --title $IssueTitle --body $body
  if ($LASTEXITCODE -ne 0) { throw "gh pr create failed" }
  Write-AttemptSidecar -Phase $activePhase -Status 'succeeded'
  $dispatchSucceeded = $true

  Log "done: $prUrl"
  [pscustomobject]@{
    ok = $true
    issue = $IssueNumber
    branch = $Branch
    run_id = $RunId
    pr = "$prUrl"
    summary = $summary
    attempt_path = $attemptPath
    status = $attemptStatus
    exit_code = $attemptExitCode
    log_bytes = $attemptLogBytes
  } | ConvertTo-Json -Compress
}
catch {
  $preserveArtifacts = $true
  $failurePhase = if ($activePhase) { $activePhase } elseif ($attemptPhase) { $attemptPhase } else { 'worktree_created' }
  Write-AttemptSidecar -Phase $failurePhase -Status 'failed' -ErrorMessage $_.Exception.Message
  Write-RecoveryBrief -FailurePhase $failurePhase -ErrorMessage $_.Exception.Message
  throw
}
finally {
  if ($dispatchSucceeded) {
    Write-AttemptSidecar -Phase 'cleanup' -Status 'succeeded'
  }
  if (-not $KeepWorktree -and -not $preserveArtifacts) {
    git -C $Repo worktree remove $wt --force 2>$null
    git -C $Repo branch -D $Branch 2>$null         # local branch only; pushed copy stays on origin for the PR
    Remove-Item -Recurse -Force $scratch -ErrorAction SilentlyContinue
  } elseif ($preserveArtifacts) {
    Log "preserved failed attempt artifacts: $scratch"
  } else {
    Log "kept worktree: $wt   (scratch: $scratch)"
  }
}
