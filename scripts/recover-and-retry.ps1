#Requires -Version 7
<#
.SYNOPSIS
  Recover a failed loopcoder worker attempt and retry with bounded backoff.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Repo,
  [Parameter(Mandatory)][int]$IssueNumber,
  [Parameter(Mandatory)][string]$IssueTitle,
  [string]$IssueBody = '',
  [Parameter(Mandatory)][string]$RunId,
  [string]$BaseBranch = 'main',
  [int]$MaxAttempts = 3,
  [int[]]$BackoffSeconds = @(10, 30, 120),
  [ValidateSet('codex')][string]$Provider = 'codex',
  [string]$Model,
  [string]$Effort
)
$ErrorActionPreference = 'Stop'

function Log($m){ Write-Host "[loopcoder] $m" }

function Get-JsonProperty {
  param(
    [AllowNull()][object]$Object,
    [string]$Name,
    [AllowNull()][object]$Default = $null
  )
  if ($null -eq $Object) { return $Default }
  $property = $Object.PSObject.Properties[$Name]
  if ($property) { return $property.Value }
  return $Default
}

function Resolve-StatePathValue {
  param([AllowNull()][string]$Path)
  if ([string]::IsNullOrWhiteSpace($Path)) { return $null }
  if ([System.IO.Path]::IsPathRooted($Path)) { return $Path }
  return (Join-Path $Repo $Path)
}

function Read-AttemptHistory {
  $history = @()
  if (-not (Test-Path -LiteralPath $workersPath -PathType Container)) {
    return @()
  }

  $attemptFiles = @(Get-ChildItem -LiteralPath $workersPath -Filter '*.attempt.json' -File -ErrorAction SilentlyContinue)
  foreach ($file in $attemptFiles) {
    try {
      $raw = Get-Content -LiteralPath $file.FullName -Raw -ErrorAction Stop
      if ([string]::IsNullOrWhiteSpace($raw)) { continue }
      $record = $raw | ConvertFrom-Json -ErrorAction Stop
      $recordIssue = Get-JsonProperty -Object $record -Name 'issue'
      if ($null -eq $recordIssue -or [int]$recordIssue -ne $IssueNumber) { continue }

      $jobId = [string](Get-JsonProperty -Object $record -Name 'job_id')
      if ([string]::IsNullOrWhiteSpace($jobId)) {
        $jobId = $file.BaseName -replace '\.attempt$',''
      }

      try {
        $attemptNumber = [int](Get-JsonProperty -Object $record -Name 'attempt' -Default 1)
      } catch {
        $attemptNumber = 1
      }

      $briefPath = Resolve-StatePathValue ([string](Get-JsonProperty -Object $record -Name 'recovery_context_path'))
      if ([string]::IsNullOrWhiteSpace($briefPath)) {
        $briefPath = Join-Path $recoveryPath "$jobId-context.md"
      }

      $history += [pscustomobject]@{
        attempt = $attemptNumber
        job_id = $jobId
        status = [string](Get-JsonProperty -Object $record -Name 'status' -Default '')
        phase = [string](Get-JsonProperty -Object $record -Name 'phase' -Default '')
        error = [string](Get-JsonProperty -Object $record -Name 'error' -Default '')
        branch = [string](Get-JsonProperty -Object $record -Name 'branch' -Default '')
        attempt_path = $file.FullName
        recovery_context_path = $briefPath
        started_at = [string](Get-JsonProperty -Object $record -Name 'started_at' -Default '')
        heartbeat_at = [string](Get-JsonProperty -Object $record -Name 'heartbeat_at' -Default '')
        last_progress_at = [string](Get-JsonProperty -Object $record -Name 'last_progress_at' -Default '')
        last_write_utc = $file.LastWriteTimeUtc
      }
    } catch {
      Write-Warning "[loopcoder] skipping unreadable attempt state $($file.FullName): $($_.Exception.Message)"
    }
  }

  return @($history | Sort-Object attempt, last_write_utc)
}

function Format-AttemptHistory {
  param([object[]]$Attempts)
  if (-not $Attempts -or $Attempts.Count -eq 0) {
    return '(no prior attempts found)'
  }

  $lines = foreach ($attempt in ($Attempts | Sort-Object attempt, last_write_utc)) {
    "attempt $($attempt.attempt) job $($attempt.job_id): status=$($attempt.status), phase=$($attempt.phase), error=$($attempt.error), sidecar=$($attempt.attempt_path), recovery=$($attempt.recovery_context_path)"
  }
  return ($lines -join [Environment]::NewLine)
}

function Read-GhPrList {
  param([string[]]$GhArgs)
  try {
    Push-Location $Repo
    try {
      $json = & gh pr list @GhArgs 2>$null
      $exitCode = $LASTEXITCODE
    } finally {
      Pop-Location
    }

    if ($exitCode -ne 0 -or [string]::IsNullOrWhiteSpace($json)) {
      return @()
    }
    return @($json | ConvertFrom-Json -ErrorAction Stop)
  } catch {
    Write-Warning "[loopcoder] gh pr list failed: $($_.Exception.Message)"
    return @()
  }
}

function Find-OpenIssuePr {
  param(
    [string[]]$CandidateBranches,
    [string]$RetryPrefix
  )

  foreach ($branch in $CandidateBranches) {
    $prs = @(Read-GhPrList -GhArgs @('--head', $branch, '--json', 'number,url'))
    if ($prs.Count -gt 0) {
      return [pscustomobject]@{
        number = $prs[0].number
        url = $prs[0].url
        headRefName = $branch
      }
    }
  }

  $openPrs = @(Read-GhPrList -GhArgs @('--state', 'open', '--json', 'number,url,headRefName'))
  foreach ($pr in $openPrs) {
    if ($pr.headRefName -eq $baseIssueBranch -or $pr.headRefName -like "$RetryPrefix*") {
      return $pr
    }
  }

  return $null
}

$Repo = (Resolve-Path -LiteralPath $Repo).Path
$runPath = Join-Path (Join-Path (Join-Path $Repo '.loopcoder') 'runs') $RunId
$workersPath = Join-Path $runPath 'workers'
$recoveryPath = Join-Path $runPath 'recovery'
$dispatchScript = Join-Path (Join-Path $Repo 'scripts') 'dispatch-worker.ps1'
$baseIssueBranch = "loop/issue-$IssueNumber"
$retryPrefix = "$baseIssueBranch-retry-"

$attempts = @(Read-AttemptHistory)
$priorAttempts = $attempts.Count
$latestAttempt = $attempts | Sort-Object attempt, last_write_utc | Select-Object -Last 1
$latestStatus = if ($latestAttempt) { $latestAttempt.status } else { 'missing-state' }
$latestBriefPath = if ($latestAttempt) { $latestAttempt.recovery_context_path } else { '' }
$latestBriefText = ''
if ($latestBriefPath -and (Test-Path -LiteralPath $latestBriefPath -PathType Leaf)) {
  try {
    $latestBriefText = Get-Content -LiteralPath $latestBriefPath -Raw -ErrorAction Stop
  } catch {
    Write-Warning "[loopcoder] failed to read recovery brief ${latestBriefPath}: $($_.Exception.Message)"
  }
}

$candidateBranches = @($baseIssueBranch)
foreach ($attempt in $attempts) {
  if (-not [string]::IsNullOrWhiteSpace($attempt.branch)) {
    $candidateBranches += $attempt.branch
  }
}
$maxRetryBranch = [Math]::Max($MaxAttempts + 1, $priorAttempts + 2)
for ($i = 2; $i -le $maxRetryBranch; $i++) {
  $candidateBranches += "$retryPrefix$i"
}
$candidateBranches = @($candidateBranches | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -Unique)

$adoptedPr = Find-OpenIssuePr -CandidateBranches $candidateBranches -RetryPrefix $retryPrefix
if ($adoptedPr) {
  Write-Host "ADOPT EXISTING PR; NO RETRY"
  Write-Host "Issue: #$IssueNumber"
  Write-Host "RunId: $RunId"
  Write-Host "Prior attempts: $priorAttempts"
  Write-Host "Latest status: $latestStatus"
  Write-Host "PR: #$($adoptedPr.number) $($adoptedPr.url)"
  Write-Host "Head branch: $($adoptedPr.headRefName)"
  exit 0
}

if ($priorAttempts -ge $MaxAttempts) {
  Write-Host "BLOCKED: retry limit reached"
  Write-Host "Issue: #$IssueNumber"
  Write-Host "RunId: $RunId"
  Write-Host "Prior attempts: $priorAttempts"
  Write-Host "Max attempts: $MaxAttempts"
  Write-Host "Latest status: $latestStatus"
  Write-Host "Latest recovery brief: $latestBriefPath"
  Write-Host ""
  Write-Host "Latest recovery brief contents:"
  Write-Host $(if ([string]::IsNullOrWhiteSpace($latestBriefText)) { '(no recovery brief available)' } else { $latestBriefText })
  Write-Host ""
  Write-Host "Attempt history:"
  Write-Host (Format-AttemptHistory -Attempts $attempts)
  Write-Host ""
  Write-Host "Human decision needed: inspect the latest recovery brief, then decide whether to fix credentials/environment, clarify the issue, raise the retry limit and dispatch manually, or close/supersede the failed branch."
  exit 1
}

$recoveryContext = $latestBriefText

$nextAttempt = $priorAttempts + 1
$backoffIndex = [Math]::Min([Math]::Max($priorAttempts - 1, 0), [Math]::Max($BackoffSeconds.Count - 1, 0))
$backoff = if ($BackoffSeconds.Count -gt 0) { $BackoffSeconds[$backoffIndex] } else { 0 }
$retryBranch = "$retryPrefix$nextAttempt"

Write-Host "RETRY: dispatching issue #$IssueNumber attempt $nextAttempt"
Write-Host "RunId: $RunId"
Write-Host "Prior attempts: $priorAttempts"
Write-Host "Latest status: $latestStatus"
Write-Host "Latest recovery brief: $latestBriefPath"
Write-Host "Retry branch: $retryBranch"
Write-Host "Backoff seconds: $backoff"

if ($backoff -gt 0) {
  Start-Sleep -Seconds $backoff
}

$dispatchArgs = @(
  '-Repo', $Repo,
  '-IssueNumber', $IssueNumber,
  '-IssueTitle', $IssueTitle,
  '-IssueBody', $IssueBody,
  '-BaseBranch', $BaseBranch,
  '-Branch', $retryBranch,
  '-RunId', $RunId,
  '-Attempt', $nextAttempt,
  '-RecoveryContext', $recoveryContext,
  '-Provider', $Provider
)
if (-not [string]::IsNullOrWhiteSpace($Model)) { $dispatchArgs += @('-Model', $Model) }
if (-not [string]::IsNullOrWhiteSpace($Effort)) { $dispatchArgs += @('-Effort', $Effort) }

& $dispatchScript @dispatchArgs
