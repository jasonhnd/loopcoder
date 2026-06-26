#Requires -Version 7
<#
.SYNOPSIS
  Compute the loopcoder ready set from GitHub first, then local advisory state.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Repo,
  [string]$BaseBranch = 'main',
  [string]$RunId,
  [ValidateSet('text', 'json', 'both')][string]$Format = 'text',
  [switch]$IncludeClosed
)
$ErrorActionPreference = 'Stop'

function Get-UtcIso { (Get-Date).ToUniversalTime().ToString('o') }

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

function Invoke-Gh {
  param([string[]]$GhArgs)

  Push-Location -LiteralPath $Repo
  try {
    $output = & gh @GhArgs 2>$null
    $exitCode = $LASTEXITCODE
  } finally {
    Pop-Location
  }

  return [pscustomobject]@{
    exit_code = $exitCode
    text = ($output -join [Environment]::NewLine)
  }
}

function Convert-GhJson {
  param(
    [object]$Result,
    [string]$Description,
    [switch]$AllowFailure
  )

  if ($Result.exit_code -ne 0) {
    if (-not $AllowFailure) {
      Write-Warning "[loopcoder] gh $Description failed with exit $($Result.exit_code)"
    }
    return @()
  }
  if ([string]::IsNullOrWhiteSpace($Result.text)) {
    return @()
  }

  $jsonText = $Result.text.Trim()
  $arrayStart = $jsonText.IndexOf('[')
  $objectStart = $jsonText.IndexOf('{')
  $starts = @($arrayStart, $objectStart) | Where-Object { $_ -ge 0 } | Sort-Object
  if ($starts.Count -gt 0 -and $starts[0] -gt 0) {
    $jsonText = $jsonText.Substring($starts[0]).Trim()
  }
  if ($jsonText.StartsWith('[')) {
    $arrayEnd = $jsonText.LastIndexOf(']')
    if ($arrayEnd -ge 0) { $jsonText = $jsonText.Substring(0, $arrayEnd + 1) }
  } elseif ($jsonText.StartsWith('{')) {
    $objectEnd = $jsonText.LastIndexOf('}')
    if ($objectEnd -ge 0) { $jsonText = $jsonText.Substring(0, $objectEnd + 1) }
  }

  try {
    return @($jsonText | ConvertFrom-Json -ErrorAction Stop)
  } catch {
    if (-not $AllowFailure) {
      Write-Warning "[loopcoder] could not parse gh $Description JSON: $($_.Exception.Message)"
    }
    return @()
  }
}

function Invoke-GhJson {
  param(
    [string[]]$GhArgs,
    [string]$Description,
    [switch]$AllowFailure
  )

  $result = Invoke-Gh -GhArgs $GhArgs
  return @(Convert-GhJson -Result $result -Description $Description -AllowFailure:$AllowFailure)
}

function Get-RepoName {
  $repoView = @(Invoke-GhJson -GhArgs @('repo', 'view', '--json', 'nameWithOwner') -Description 'repo view' -AllowFailure)
  if ($repoView.Count -gt 0) {
    $nameWithOwner = [string](Get-JsonProperty -Object $repoView[0] -Name 'nameWithOwner' -Default '')
    if (-not [string]::IsNullOrWhiteSpace($nameWithOwner)) { return $nameWithOwner }
  }

  try {
    $remote = @(git -C $Repo remote get-url origin 2>$null) | Select-Object -First 1
    if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($remote)) {
      $cleanRemote = ([string]$remote).Trim()
      if ($cleanRemote -match 'github\.com[:/](?<owner>[^/]+)/(?<name>.+?)(?:\.git)?$') {
        return "$($Matches.owner)/$($Matches.name)"
      }
      return $cleanRemote
    }
  } catch {}

  return $Repo
}

function Convert-ToRepoRelativePath {
  param([AllowNull()][string]$Path)
  if ([string]::IsNullOrWhiteSpace($Path)) { return '' }

  try {
    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $repoRoot = [System.IO.Path]::GetFullPath($Repo).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
    if ($fullPath.StartsWith($repoRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
      $relative = $fullPath.Substring($repoRoot.Length).TrimStart([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
      return ($relative -replace '\\', '/')
    }
  } catch {}

  return ($Path -replace '\\', '/')
}

function Get-LabelNames {
  param([AllowNull()][object]$Issue)
  $labels = @(Get-JsonProperty -Object $Issue -Name 'labels' -Default @())
  return @($labels | ForEach-Object {
    if ($_ -is [string]) {
      $_
    } else {
      [string](Get-JsonProperty -Object $_ -Name 'name' -Default '')
    }
  } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Get-BlockedDependencyNumbers {
  param([string[]]$Labels)

  $numbers = @()
  foreach ($label in $Labels) {
    if ($label -match '(?i)^blocked-by:\s*#(\d+)$') {
      $numbers += [int]$Matches[1]
    }
  }
  return @($numbers | Select-Object -Unique | Sort-Object)
}

function Get-ConfiguredThresholds {
  $config = [ordered]@{
    heartbeat_interval_seconds = 15
    stale_after_seconds = 120
    hung_after_seconds = 300
  }

  $configPath = Join-Path $Repo '.delivery.yml'
  if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)) {
    return [pscustomobject]$config
  }

  try {
    $inResilience = $false
    $inWorker = $false
    foreach ($rawLine in (Get-Content -LiteralPath $configPath -ErrorAction Stop)) {
      if ($rawLine -match '^\s*#' -or [string]::IsNullOrWhiteSpace($rawLine)) { continue }

      if ($rawLine -match '^resilience\s*:') {
        $inResilience = $true
        $inWorker = $false
        continue
      }
      if ($inResilience -and $rawLine -match '^\S' -and $rawLine -notmatch '^resilience\s*:') {
        $inResilience = $false
        $inWorker = $false
      }
      if ($inResilience -and $rawLine -match '^\s+worker\s*:') {
        $inWorker = $true
        continue
      }
      if ($inWorker -and $rawLine -match '^\s+(heartbeat_interval_seconds|stale_after_seconds|hung_after_seconds)\s*:\s*(\d+)') {
        $config[$Matches[1]] = [int]$Matches[2]
      }
    }
  } catch {
    Write-Warning "[loopcoder] could not read .delivery.yml resilience thresholds: $($_.Exception.Message)"
  }

  return [pscustomobject]$config
}

function Get-IssueNumberFromText {
  param([AllowNull()][string]$Text)
  if ([string]::IsNullOrWhiteSpace($Text)) { return $null }

  $branchMatch = [regex]::Match($Text, '(?i)(?:^|[\/_-])issue[\/_-]?(\d+)(?:\D|$)')
  if ($branchMatch.Success) { return [int]$branchMatch.Groups[1].Value }

  $hashMatch = [regex]::Match($Text, '#(\d+)')
  if ($hashMatch.Success) { return [int]$hashMatch.Groups[1].Value }

  return $null
}

function Get-CandidateBranches {
  param(
    [int]$IssueNumber,
    [object[]]$Attempts
  )

  $branches = @("loop/issue-$IssueNumber")
  foreach ($attempt in $Attempts) {
    if (-not [string]::IsNullOrWhiteSpace($attempt.branch)) {
      $branches += $attempt.branch
    }
    if ($attempt.attempt -gt 1) {
      $branches += "loop/issue-$IssueNumber-retry-$($attempt.attempt)"
    }
  }
  return @($branches | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -Unique)
}

function Get-AgeSeconds {
  param([AllowNull()][string]$Timestamp)
  if ([string]::IsNullOrWhiteSpace($Timestamp)) { return $null }

  $parsed = [datetimeoffset]::MinValue
  if ([datetimeoffset]::TryParse($Timestamp, [ref]$parsed)) {
    return [Math]::Max(0, ($script:nowUtc - $parsed.UtcDateTime).TotalSeconds)
  }
  return $null
}

function Test-PidLiveness {
  param([AllowNull()][object]$PidValue)
  if ($null -eq $PidValue -or [string]::IsNullOrWhiteSpace([string]$PidValue)) {
    return [pscustomobject]@{ alive = $false; state = 'unknown'; evidence = 'pid missing' }
  }

  try {
    $pidNumber = [int]$PidValue
  } catch {
    return [pscustomobject]@{ alive = $false; state = 'unknown'; evidence = "pid invalid: $PidValue" }
  }

  try {
    Get-Process -Id $pidNumber -ErrorAction Stop | Out-Null
    return [pscustomobject]@{ alive = $true; state = 'alive'; evidence = "pid $pidNumber alive on this host" }
  } catch {
    return [pscustomobject]@{ alive = $false; state = 'missing'; evidence = "pid $pidNumber not found on this host" }
  }
}

function Get-CheckCount {
  param(
    [hashtable]$Counts,
    [string]$Name
  )
  if ($Counts.ContainsKey($Name)) { return [int]$Counts[$Name] }
  return 0
}

function Get-CheckSummary {
  param(
    [object[]]$Checks,
    [int]$ExitCode
  )

  if (-not $Checks -or $Checks.Count -eq 0) {
    $state = if ($ExitCode -eq 0) { 'none' } else { 'unknown' }
    $text = if ($ExitCode -eq 0) { 'no checks reported' } else { "checks unavailable (gh exit $ExitCode)" }
    return [pscustomobject]@{ state = $state; text = $text }
  }

  $counts = @{}
  foreach ($check in $Checks) {
    $bucket = [string](Get-JsonProperty -Object $check -Name 'bucket' -Default '')
    if ([string]::IsNullOrWhiteSpace($bucket)) {
      $bucket = [string](Get-JsonProperty -Object $check -Name 'state' -Default 'unknown')
    }
    if ([string]::IsNullOrWhiteSpace($bucket)) { $bucket = 'unknown' }
    if (-not $counts.ContainsKey($bucket)) { $counts[$bucket] = 0 }
    $counts[$bucket]++
  }

  $summaryParts = @($counts.GetEnumerator() | Sort-Object Name | ForEach-Object { "$($_.Name)=$($_.Value)" })
  $state = 'unknown'
  if (((Get-CheckCount -Counts $counts -Name 'fail') + (Get-CheckCount -Counts $counts -Name 'cancel')) -gt 0) {
    $state = 'fail'
  } elseif ((Get-CheckCount -Counts $counts -Name 'pending') -gt 0) {
    $state = 'pending'
  } elseif (((Get-CheckCount -Counts $counts -Name 'pass') + (Get-CheckCount -Counts $counts -Name 'skipping')) -gt 0) {
    $state = 'pass'
  }

  return [pscustomobject]@{
    state = $state
    text = ($summaryParts -join ', ')
  }
}

function Get-PrSubState {
  param(
    [AllowNull()][object]$Pr,
    [AllowNull()][object]$LatestAttempt
  )
  if ($null -eq $Pr) { return $null }

  if ($LatestAttempt -and [string]$LatestAttempt.status -eq 'running') {
    return 'adopt-PR'
  }
  if ([bool](Get-JsonProperty -Object $Pr -Name 'isDraft' -Default $false)) {
    return 'gated'
  }
  if ($Pr.check_summary.state -eq 'fail') { return 'fixing' }
  if ($Pr.check_summary.state -eq 'pending' -or $Pr.check_summary.state -eq 'unknown') { return 'gated' }
  return 'in-review'
}

function Get-ClosingPrRefs {
  param([AllowNull()][object]$Issue)
  $refs = @(Get-JsonProperty -Object $Issue -Name 'closedByPullRequestsReferences' -Default @())
  return @($refs | Where-Object { $null -ne $_ })
}

function Test-ClosingPrMerged {
  param([AllowNull()][object]$PrRef)
  if ($null -eq $PrRef) { return $false }

  $state = [string](Get-JsonProperty -Object $PrRef -Name 'state' -Default '')
  $mergedAt = [string](Get-JsonProperty -Object $PrRef -Name 'mergedAt' -Default '')
  $merged = Get-JsonProperty -Object $PrRef -Name 'merged'
  if ($state -eq 'MERGED' -or -not [string]::IsNullOrWhiteSpace($mergedAt) -or $merged -eq $true) {
    return $true
  }

  $number = Get-JsonProperty -Object $PrRef -Name 'number'
  if ($null -eq $number) { return $false }

  $cacheKey = [string]$number
  if ($script:closingPrMergeCache.ContainsKey($cacheKey)) {
    return [bool]$script:closingPrMergeCache[$cacheKey]
  }

  $details = @(Invoke-GhJson -GhArgs @('pr', 'view', [string]$number, '--json', 'number,state,mergedAt') -Description "pr view $number" -AllowFailure)
  $isMerged = $false
  if ($details.Count -gt 0) {
    $detail = $details[0]
    $detailState = [string](Get-JsonProperty -Object $detail -Name 'state' -Default '')
    $detailMergedAt = [string](Get-JsonProperty -Object $detail -Name 'mergedAt' -Default '')
    $isMerged = ($detailState -eq 'MERGED' -or -not [string]::IsNullOrWhiteSpace($detailMergedAt))
  }
  $script:closingPrMergeCache[$cacheKey] = $isMerged
  return $isMerged
}

function Get-DependencyStatus {
  param([int]$Number)

  if (-not $script:dependencyCache.ContainsKey([string]$Number)) {
    $details = @(Invoke-GhJson -GhArgs @('issue', 'view', [string]$Number, '--json', 'number,title,state,stateReason,closedByPullRequestsReferences,labels') -Description "issue view $Number" -AllowFailure)
    if ($details.Count -gt 0) {
      $script:dependencyCache[[string]$Number] = $details[0]
    } else {
      $script:dependencyCache[[string]$Number] = [pscustomobject]@{
        number = $Number
        title = '(issue details unavailable)'
        state = 'UNKNOWN'
        stateReason = ''
        labels = @()
        closedByPullRequestsReferences = @()
      }
    }
  }

  $issue = $script:dependencyCache[[string]$Number]
  $state = [string](Get-JsonProperty -Object $issue -Name 'state' -Default 'UNKNOWN')
  $stateReason = [string](Get-JsonProperty -Object $issue -Name 'stateReason' -Default '')
  $closingRefs = @(Get-ClosingPrRefs -Issue $issue)
  $closedAsCompleted = ($state -eq 'CLOSED' -and $stateReason -eq 'COMPLETED')
  $mergedClosingPr = $false
  foreach ($ref in $closingRefs) {
    if (Test-ClosingPrMerged -PrRef $ref) {
      $mergedClosingPr = $true
      break
    }
  }

  $completed = ($closedAsCompleted -or $mergedClosingPr)
  $reason = if ($completed) {
    if ($closedAsCompleted) {
      "#$Number is closed as completed"
    } else {
      "#$Number has a merged closing PR"
    }
  } elseif ($state -eq 'UNKNOWN') {
    "#$Number state is unknown"
  } elseif ($state -eq 'OPEN') {
    "#$Number is still open"
  } elseif ($state -eq 'CLOSED') {
    if ([string]::IsNullOrWhiteSpace($stateReason)) {
      "#$Number is closed without a completed state reason"
    } else {
      "#$Number is closed as $stateReason"
    }
  } else {
    "#$Number state is $state"
  }

  return [pscustomobject]@{
    number = $Number
    title = [string](Get-JsonProperty -Object $issue -Name 'title' -Default '')
    state = $state
    stateReason = $stateReason
    completed = $completed
    reason = $reason
  }
}

function New-IssueRecord {
  param(
    [int]$Number,
    [string]$Title,
    [string]$State,
    [string]$StateReason = '',
    [object[]]$Labels = @()
  )

  return [pscustomobject]@{
    number = $Number
    title = $Title
    state = $State
    stateReason = $StateReason
    labels = $Labels
  }
}

function Read-LocalAttempts {
  param([AllowNull()][string]$WorkersPath)
  if ([string]::IsNullOrWhiteSpace($WorkersPath) -or -not (Test-Path -LiteralPath $WorkersPath -PathType Container)) {
    return @()
  }

  $records = @()
  $files = @(Get-ChildItem -LiteralPath $WorkersPath -Filter '*.attempt.json' -File -ErrorAction SilentlyContinue)
  foreach ($file in $files) {
    try {
      $raw = Get-Content -LiteralPath $file.FullName -Raw -ErrorAction Stop
      if ([string]::IsNullOrWhiteSpace($raw)) { continue }
      $json = $raw | ConvertFrom-Json -ErrorAction Stop
      $issue = Get-JsonProperty -Object $json -Name 'issue'
      if ($null -eq $issue) {
        Write-Warning "[loopcoder] skipping attempt without issue number: $($file.FullName)"
        continue
      }

      $attemptNumber = 1
      try {
        $attemptNumber = [int](Get-JsonProperty -Object $json -Name 'attempt' -Default 1)
      } catch {}

      $branch = [string](Get-JsonProperty -Object $json -Name 'branch' -Default '')
      if ([string]::IsNullOrWhiteSpace($branch)) {
        if ($attemptNumber -gt 1) {
          $branch = "loop/issue-$issue-retry-$attemptNumber"
        } else {
          $branch = "loop/issue-$issue"
        }
      }

      $jobId = [string](Get-JsonProperty -Object $json -Name 'job_id' -Default '')
      if ([string]::IsNullOrWhiteSpace($jobId)) {
        $jobId = $file.BaseName -replace '\.attempt$', ''
      }

      $records += [pscustomobject]@{
        issue = [int]$issue
        job_id = $jobId
        attempt = $attemptNumber
        provider = [string](Get-JsonProperty -Object $json -Name 'provider' -Default '')
        pid = Get-JsonProperty -Object $json -Name 'pid'
        phase = [string](Get-JsonProperty -Object $json -Name 'phase' -Default '')
        status = [string](Get-JsonProperty -Object $json -Name 'status' -Default '')
        branch = $branch
        started_at = [string](Get-JsonProperty -Object $json -Name 'started_at' -Default '')
        heartbeat_at = [string](Get-JsonProperty -Object $json -Name 'heartbeat_at' -Default '')
        last_progress_at = [string](Get-JsonProperty -Object $json -Name 'last_progress_at' -Default '')
        exit_code = Get-JsonProperty -Object $json -Name 'exit_code'
        error = [string](Get-JsonProperty -Object $json -Name 'error' -Default '')
        path = $file.FullName
        last_write_utc = $file.LastWriteTimeUtc
      }
    } catch {
      Write-Warning "[loopcoder] skipping unreadable attempt state $($file.FullName): $($_.Exception.Message)"
    }
  }

  return @($records | Sort-Object issue, attempt, last_write_utc)
}

function Convert-AttemptSummary {
  param([object[]]$Attempts)

  return @($Attempts | Sort-Object attempt, last_write_utc | ForEach-Object {
    [pscustomobject]@{
      job_id = $_.job_id
      attempt = $_.attempt
      status = $_.status
      phase = $_.phase
      pid = $_.pid
      branch = $_.branch
      path = Convert-ToRepoRelativePath -Path $_.path
      heartbeat_at = $_.heartbeat_at
      last_progress_at = $_.last_progress_at
    }
  })
}

function Get-LocalAttemptDisposition {
  param(
    [AllowNull()][object]$Attempt,
    [object]$Thresholds,
    [int]$HeartbeatFreshSeconds
  )
  if ($null -eq $Attempt) { return $null }

  $status = ([string]$Attempt.status).ToLowerInvariant()
  $phase = ([string]$Attempt.phase).ToLowerInvariant()
  $heartbeatAge = Get-AgeSeconds -Timestamp $Attempt.heartbeat_at
  $progressAge = Get-AgeSeconds -Timestamp $Attempt.last_progress_at
  $pidState = Test-PidLiveness -PidValue $Attempt.pid
  $heartbeatFresh = ($null -ne $heartbeatAge -and $heartbeatAge -le $HeartbeatFreshSeconds)
  $heartbeatStale = ($null -ne $heartbeatAge -and $heartbeatAge -gt $HeartbeatFreshSeconds)
  $progressStale = ($null -ne $progressAge -and $progressAge -gt [int]$Thresholds.stale_after_seconds)
  $progressHung = ($null -ne $progressAge -and $progressAge -gt [int]$Thresholds.hung_after_seconds)

  if ($status -in @('failed', 'failure', 'error', 'canceled', 'cancelled', 'timeout', 'timed_out')) {
    return [pscustomobject]@{
      classification = 'recovery-needed'
      reason = "latest attempt $($Attempt.job_id) failed with status '$($Attempt.status)'"
      pid = $pidState
    }
  }

  if ($status -in @('succeeded', 'success', 'completed', 'complete', 'done')) {
    return [pscustomobject]@{
      classification = 'recovery-needed'
      reason = "latest attempt $($Attempt.job_id) completed locally but no closed issue or open PR was found"
      pid = $pidState
    }
  }

  if ($status -eq 'idle' -or $phase -match 'idle|waiting') {
    return [pscustomobject]@{
      classification = 'recovery-needed'
      reason = "latest attempt $($Attempt.job_id) is idle and needs recovery review"
      pid = $pidState
    }
  }

  if ($status -eq 'running' -or [string]::IsNullOrWhiteSpace($status)) {
    if ($progressHung) {
      if ($pidState.alive) {
        return [pscustomobject]@{
          classification = 'has-live-attempt'
          reason = "latest attempt $($Attempt.job_id) is hung but pid is still alive"
          pid = $pidState
        }
      }
      return [pscustomobject]@{
        classification = 'recovery-needed'
        reason = "latest attempt $($Attempt.job_id) is hung with no live pid"
        pid = $pidState
      }
    }

    if ($progressStale) {
      return [pscustomobject]@{
        classification = 'has-live-attempt'
        reason = "latest attempt $($Attempt.job_id) progress is stale"
        pid = $pidState
      }
    }

    if ($heartbeatFresh) {
      return [pscustomobject]@{
        classification = 'has-live-attempt'
        reason = "latest attempt $($Attempt.job_id) is running with a fresh heartbeat"
        pid = $pidState
      }
    }

    if ($pidState.alive) {
      return [pscustomobject]@{
        classification = 'has-live-attempt'
        reason = "latest attempt $($Attempt.job_id) has a live pid"
        pid = $pidState
      }
    }

    if ($heartbeatStale -or $null -eq $heartbeatAge) {
      return [pscustomobject]@{
        classification = 'recovery-needed'
        reason = "latest attempt $($Attempt.job_id) is orphaned with no fresh heartbeat or live pid"
        pid = $pidState
      }
    }
  }

  return [pscustomobject]@{
    classification = 'recovery-needed'
    reason = "latest attempt $($Attempt.job_id) has status '$($Attempt.status)' and needs recovery review"
    pid = $pidState
  }
}

function Convert-PrSummary {
  param(
    [object[]]$Prs,
    [AllowNull()][object]$LatestAttempt
  )

  return @($Prs | Sort-Object number -Unique | ForEach-Object {
    [pscustomobject]@{
      number = [int]$_.number
      url = [string]$_.url
      head = [string]$_.headRefName
      sub_state = Get-PrSubState -Pr $_ -LatestAttempt $LatestAttempt
    }
  })
}

function Format-DependencyReason {
  param([object[]]$UnsatisfiedDependencies)

  if ($UnsatisfiedDependencies.Count -eq 1) {
    return "blocked by $($UnsatisfiedDependencies[0].reason)"
  }

  $parts = @($UnsatisfiedDependencies | ForEach-Object { $_.reason })
  return "blocked by dependencies: $($parts -join '; ')"
}

function Write-ReadySetText {
  param([object]$Report)

  Write-Output 'READY SET'
  Write-Output "Repo: $($Report.repo)"
  Write-Output "Base branch: $($Report.base_branch)"
  Write-Output "RunId: $(if ([string]::IsNullOrWhiteSpace($Report.run_id)) { '(none)' } else { $Report.run_id })"
  Write-Output "Generated at: $($Report.generated_at)"
  Write-Output ''
  Write-Output 'Ready'
  if ($Report.ready.Count -eq 0) {
    Write-Output '- none'
  } else {
    foreach ($item in $Report.ready) {
      Write-Output "- #$($item.issue) $($item.title)"
      Write-Output "  reason: $($item.reason)"
    }
  }

  Write-Output ''
  Write-Output 'Non-ready'
  if ($Report.blocked.Count -eq 0) {
    Write-Output '- none'
  } else {
    foreach ($item in $Report.blocked) {
      Write-Output "- #$($item.issue) $($item.title)"
      Write-Output "  classification: $($item.classification)"
      Write-Output "  reason: $($item.reason)"
    }
  }

  Write-Output ''
  Write-Output 'Safety'
  Write-Output '- ready-set is read-only: no dispatch, no merge, no push, and no GitHub mutation was attempted.'
}

$Repo = (Resolve-Path -LiteralPath $Repo).Path
$script:nowUtc = (Get-Date).ToUniversalTime()
$script:dependencyCache = @{}
$script:closingPrMergeCache = @{}
$repoName = Get-RepoName
$generatedAt = Get-UtcIso
$thresholds = Get-ConfiguredThresholds
$heartbeatFreshSeconds = [int]$thresholds.heartbeat_interval_seconds * 2

# GitHub first: candidate issues, dependency issues, open PRs, and PR checks.
$issueState = if ($IncludeClosed) { 'all' } else { 'open' }
$issueResult = Invoke-Gh -GhArgs @('issue', 'list', '--state', $issueState, '--limit', '1000', '--json', 'number,title,state,labels,stateReason')
$rawIssues = @(Convert-GhJson -Result $issueResult -Description "issue list --state $issueState" -AllowFailure:$false)

$issueRecords = @()
foreach ($issue in $rawIssues) {
  $state = [string](Get-JsonProperty -Object $issue -Name 'state' -Default '')
  if ([string]::IsNullOrWhiteSpace($state)) { $state = if ($IncludeClosed) { 'UNKNOWN' } else { 'OPEN' } }
  $issueRecords += New-IssueRecord `
    -Number ([int]$issue.number) `
    -Title ([string]$issue.title) `
    -State $state `
    -StateReason ([string](Get-JsonProperty -Object $issue -Name 'stateReason' -Default '')) `
    -Labels @(Get-LabelNames -Issue $issue)
}

$dependencyNumbers = @()
foreach ($issue in $issueRecords) {
  $dependencyNumbers += @(Get-BlockedDependencyNumbers -Labels $issue.labels)
}
$dependencyNumbers = @($dependencyNumbers | Select-Object -Unique | Sort-Object)
foreach ($dependencyNumber in $dependencyNumbers) {
  Get-DependencyStatus -Number $dependencyNumber | Out-Null
}

$openPrResult = Invoke-Gh -GhArgs @('pr', 'list', '--state', 'open', '--limit', '1000', '--json', 'number,title,url,headRefName,isDraft,closingIssuesReferences')
$rawOpenPrs = @(Convert-GhJson -Result $openPrResult -Description 'pr list --state open' -AllowFailure:$false)

$openPrs = @()
foreach ($pr in $rawOpenPrs) {
  $checksResult = Invoke-Gh -GhArgs @('pr', 'checks', [string]$pr.number, '--json', 'name,state,bucket,link,workflow')
  $checks = @(Convert-GhJson -Result $checksResult -Description "pr checks $($pr.number)" -AllowFailure)
  $checkSummary = Get-CheckSummary -Checks $checks -ExitCode $checksResult.exit_code

  $issueNumbers = @()
  foreach ($ref in @(Get-JsonProperty -Object $pr -Name 'closingIssuesReferences' -Default @())) {
    $refNumber = Get-JsonProperty -Object $ref -Name 'number'
    if ($null -ne $refNumber) { $issueNumbers += [int]$refNumber }
  }
  $branchIssue = Get-IssueNumberFromText -Text ([string]$pr.headRefName)
  if ($null -ne $branchIssue) { $issueNumbers += [int]$branchIssue }
  $titleIssue = Get-IssueNumberFromText -Text ([string]$pr.title)
  if ($null -ne $titleIssue) { $issueNumbers += [int]$titleIssue }
  $issueNumbers = @($issueNumbers | Where-Object { $_ -gt 0 } | Select-Object -Unique)

  $openPrs += [pscustomobject]@{
    number = [int]$pr.number
    title = [string]$pr.title
    headRefName = [string]$pr.headRefName
    url = [string](Get-JsonProperty -Object $pr -Name 'url' -Default '')
    isDraft = [bool](Get-JsonProperty -Object $pr -Name 'isDraft' -Default $false)
    issue_numbers = $issueNumbers
    check_summary = $checkSummary
  }
}

# Local run state is advisory and is read only after the GitHub snapshot.
$runsRoot = Join-Path (Join-Path $Repo '.loopcoder') 'runs'
if ([string]::IsNullOrWhiteSpace($RunId)) {
  if (Test-Path -LiteralPath $runsRoot -PathType Container) {
    $latestRun = Get-ChildItem -LiteralPath $runsRoot -Directory -ErrorAction SilentlyContinue |
      Sort-Object LastWriteTimeUtc -Descending |
      Select-Object -First 1
    if ($latestRun) { $RunId = $latestRun.Name }
  }
}

$runPath = if ([string]::IsNullOrWhiteSpace($RunId)) { $null } else { Join-Path $runsRoot $RunId }
$workersPath = if ($runPath) { Join-Path $runPath 'workers' } else { $null }
$attempts = @(Read-LocalAttempts -WorkersPath $workersPath)

$attemptsByIssue = @{}
foreach ($attempt in $attempts) {
  $key = [string]$attempt.issue
  if (-not $attemptsByIssue.ContainsKey($key)) { $attemptsByIssue[$key] = @() }
  $attemptsByIssue[$key] = @($attemptsByIssue[$key] + $attempt)
}

$prsByIssue = @{}
$prsByBranch = @{}
foreach ($pr in $openPrs) {
  if (-not [string]::IsNullOrWhiteSpace($pr.headRefName)) {
    $prsByBranch[$pr.headRefName] = $pr
  }
  foreach ($issueNumber in @($pr.issue_numbers)) {
    $key = [string]$issueNumber
    if (-not $prsByIssue.ContainsKey($key)) { $prsByIssue[$key] = @() }
    $prsByIssue[$key] = @($prsByIssue[$key] + $pr)
  }
}

$ready = @()
$blocked = @()

foreach ($issue in ($issueRecords | Sort-Object number)) {
  $number = [int]$issue.number
  $key = [string]$number
  $issueAttempts = if ($attemptsByIssue.ContainsKey($key)) { @($attemptsByIssue[$key]) } else { @() }
  $latestAttempt = $issueAttempts | Sort-Object attempt, last_write_utc | Select-Object -Last 1
  $candidateBranches = Get-CandidateBranches -IssueNumber $number -Attempts $issueAttempts

  $matchingPrs = @()
  if ($prsByIssue.ContainsKey($key)) { $matchingPrs += @($prsByIssue[$key]) }
  foreach ($branch in $candidateBranches) {
    if ($prsByBranch.ContainsKey($branch)) { $matchingPrs += $prsByBranch[$branch] }
  }
  $matchingPrs = @($matchingPrs | Sort-Object number -Unique)

  $dependencies = @(Get-BlockedDependencyNumbers -Labels $issue.labels)
  $dependencyStatuses = @($dependencies | ForEach-Object { Get-DependencyStatus -Number $_ })
  $unsatisfiedDependencies = @($dependencyStatuses | Where-Object { -not $_.completed })
  $attemptSummaries = @(Convert-AttemptSummary -Attempts $issueAttempts)
  $openPrSummaries = @(Convert-PrSummary -Prs $matchingPrs -LatestAttempt $latestAttempt)

  if ($issue.state -ne 'OPEN') {
    if ($IncludeClosed) {
      $blocked += [pscustomobject]@{
        issue = $number
        title = $issue.title
        classification = 'closed'
        reason = "issue state is $($issue.state); ready-set dispatch candidates must be open"
        dependencies = $dependencies
        open_prs = $openPrSummaries
        attempts = $attemptSummaries
      }
    }
    continue
  }

  if ($unsatisfiedDependencies.Count -gt 0) {
    $blocked += [pscustomobject]@{
      issue = $number
      title = $issue.title
      classification = 'blocked-by-unmerged-dep'
      reason = Format-DependencyReason -UnsatisfiedDependencies $unsatisfiedDependencies
      dependencies = $dependencies
      open_prs = $openPrSummaries
      attempts = $attemptSummaries
    }
    continue
  }

  if ($matchingPrs.Count -gt 0) {
    $primaryPr = $matchingPrs | Sort-Object number | Select-Object -First 1
    $subState = Get-PrSubState -Pr $primaryPr -LatestAttempt $latestAttempt
    $reason = if ($subState -eq 'adopt-PR') {
      "open PR #$($primaryPr.number) exists for $($primaryPr.headRefName); local attempt should adopt it"
    } else {
      "open PR #$($primaryPr.number) exists for $($primaryPr.headRefName)"
    }
    $blocked += [pscustomobject]@{
      issue = $number
      title = $issue.title
      classification = 'has-open-PR'
      reason = $reason
      dependencies = $dependencies
      open_prs = $openPrSummaries
      attempts = $attemptSummaries
    }
    continue
  }

  $attemptDisposition = Get-LocalAttemptDisposition -Attempt $latestAttempt -Thresholds $thresholds -HeartbeatFreshSeconds $heartbeatFreshSeconds
  if ($attemptDisposition) {
    $blocked += [pscustomobject]@{
      issue = $number
      title = $issue.title
      classification = $attemptDisposition.classification
      reason = $attemptDisposition.reason
      dependencies = $dependencies
      open_prs = $openPrSummaries
      attempts = $attemptSummaries
    }
    continue
  }

  $ready += [pscustomobject]@{
    issue = $number
    title = $issue.title
    reason = 'dependencies completed; no open PR; no live local attempt'
  }
}

$summary = [ordered]@{
  ready_count = @($ready).Count
  blocked_count = @($blocked).Count
  blocked_by_unmerged_dep_count = @($blocked | Where-Object { $_.classification -eq 'blocked-by-unmerged-dep' }).Count
  has_open_pr_count = @($blocked | Where-Object { $_.classification -eq 'has-open-PR' }).Count
  has_live_attempt_count = @($blocked | Where-Object { $_.classification -eq 'has-live-attempt' }).Count
  recovery_needed_count = @($blocked | Where-Object { $_.classification -eq 'recovery-needed' }).Count
}

$report = [ordered]@{
  version = 1
  repo = $repoName
  repo_path = ($Repo -replace '\\', '/')
  base_branch = $BaseBranch
  run_id = if ([string]::IsNullOrWhiteSpace($RunId)) { $null } else { $RunId }
  generated_at = $generatedAt
  ready = @($ready)
  blocked = @($blocked)
  summary = [pscustomobject]$summary
}

if ($Format -eq 'text' -or $Format -eq 'both') {
  Write-ReadySetText -Report ([pscustomobject]$report)
}

if ($Format -eq 'both') {
  Write-Output ''
}

if ($Format -eq 'json' -or $Format -eq 'both') {
  Write-Output (($report | ConvertTo-Json -Depth 10))
}
