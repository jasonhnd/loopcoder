#Requires -Version 7
<#
.SYNOPSIS
  Reconcile a loopcoder run from GitHub first, then local advisory state.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Repo,
  [string]$RunId,
  [string]$BaseBranch = 'main'
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

  Push-Location $Repo
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

  if ($Result.exit_code -ne 0 -and -not $AllowFailure) {
    Write-Warning "[loopcoder] gh $Description failed with exit $($Result.exit_code)"
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
    Write-Warning "[loopcoder] could not parse gh $Description JSON: $($_.Exception.Message)"
    return @()
  }
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

function Format-Age {
  param([AllowNull()][object]$AgeSeconds)
  if ($null -eq $AgeSeconds) { return 'unknown' }
  return ('{0:N0}s' -f [double]$AgeSeconds)
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

function Test-RemoteBranch {
  param([AllowNull()][string]$Branch)
  if ([string]::IsNullOrWhiteSpace($Branch)) {
    return [pscustomobject]@{ state = 'unknown'; evidence = 'branch missing' }
  }

  try {
    $output = @(git -C $Repo ls-remote --heads origin $Branch 2>$null)
    if ($LASTEXITCODE -ne 0) {
      return [pscustomobject]@{ state = 'unknown'; evidence = "git ls-remote failed for $Branch" }
    }
    if ($output.Count -gt 0) {
      return [pscustomobject]@{ state = 'found'; evidence = "remote branch $Branch exists" }
    }
    return [pscustomobject]@{ state = 'missing'; evidence = "remote branch $Branch not found" }
  } catch {
    return [pscustomobject]@{ state = 'unknown'; evidence = "remote branch check failed for ${Branch}: $($_.Exception.Message)" }
  }
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
  if (($counts['fail'] + $counts['cancel']) -gt 0) {
    $state = 'fail'
  } elseif ($counts['pending'] -gt 0) {
    $state = 'pending'
  } elseif (($counts['pass'] + $counts['skipping']) -gt 0) {
    $state = 'pass'
  }

  return [pscustomobject]@{
    state = $state
    text = ($summaryParts -join ', ')
  }
}

function Get-PrClassification {
  param([AllowNull()][object]$Pr)
  if ($null -eq $Pr) { return $null }
  if ($Pr.check_summary.state -eq 'fail') { return 'fixing' }
  if ($Pr.check_summary.state -eq 'pending' -or $Pr.check_summary.state -eq 'unknown') { return 'gated' }
  return 'in-review'
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
        $jobId = $file.BaseName -replace '\.attempt$',''
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

function Read-EventCount {
  param([AllowNull()][string]$EventsPath)
  if ([string]::IsNullOrWhiteSpace($EventsPath) -or -not (Test-Path -LiteralPath $EventsPath -PathType Leaf)) {
    return 0
  }
  try {
    return @((Get-Content -LiteralPath $EventsPath -ErrorAction Stop) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }).Count
  } catch {
    Write-Warning "[loopcoder] could not read events file ${EventsPath}: $($_.Exception.Message)"
    return 0
  }
}

function Get-ClosingPrRefs {
  param([AllowNull()][object]$Issue)
  $refs = @(Get-JsonProperty -Object $Issue -Name 'closedByPullRequestsReferences' -Default @())
  return @($refs | Where-Object { $null -ne $_ })
}

function Get-BlockedLabels {
  param([string[]]$Labels)
  return @($Labels | Where-Object {
    $_ -match '(?i)^blocked-by:#\d+$' -or $_ -match '(?i)blocked|needs-human|needs:human'
  })
}

function New-IssueRecord {
  param(
    [int]$Number,
    [string]$Title,
    [string]$State,
    [string]$StateReason = '',
    [object[]]$Labels,
    [object[]]$ClosedByPullRequestsReferences = @()
  )
  return [pscustomobject]@{
    number = $Number
    title = $Title
    state = $State
    stateReason = $StateReason
    labels = $Labels
    closedByPullRequestsReferences = $ClosedByPullRequestsReferences
  }
}

$Repo = (Resolve-Path -LiteralPath $Repo).Path
$script:nowUtc = (Get-Date).ToUniversalTime()
$thresholds = Get-ConfiguredThresholds
$heartbeatFreshSeconds = [int]$thresholds.heartbeat_interval_seconds * 2

# GitHub first: issues, PRs, and PR checks are read before local sidecars.
$openIssueResult = Invoke-Gh -GhArgs @('issue', 'list', '--state', 'open', '--limit', '1000', '--json', 'number,title,labels,stateReason')
$openIssues = @(Convert-GhJson -Result $openIssueResult -Description 'issue list --state open' -AllowFailure:$false)

$openPrResult = Invoke-Gh -GhArgs @('pr', 'list', '--state', 'open', '--limit', '1000', '--json', 'number,headRefName,title,url,isDraft,closingIssuesReferences')
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
  $issueNumbers = @($issueNumbers | Select-Object -Unique)

  $openPrs += [pscustomobject]@{
    number = [int]$pr.number
    title = [string]$pr.title
    headRefName = [string]$pr.headRefName
    url = [string](Get-JsonProperty -Object $pr -Name 'url' -Default '')
    isDraft = [bool](Get-JsonProperty -Object $pr -Name 'isDraft' -Default $false)
    issue_numbers = $issueNumbers
    checks = $checks
    check_summary = $checkSummary
  }
}

$runsRoot = Join-Path (Join-Path $Repo '.loopcoder') 'runs'
$selectedRunNote = ''
if ([string]::IsNullOrWhiteSpace($RunId)) {
  if (Test-Path -LiteralPath $runsRoot -PathType Container) {
    $latestRun = Get-ChildItem -LiteralPath $runsRoot -Directory -ErrorAction SilentlyContinue |
      Sort-Object LastWriteTimeUtc -Descending |
      Select-Object -First 1
    if ($latestRun) {
      $RunId = $latestRun.Name
      $selectedRunNote = 'latest modified run selected'
    } else {
      $selectedRunNote = 'no run directories found'
    }
  } else {
    $selectedRunNote = '.loopcoder/runs not found'
  }
} else {
  $selectedRunNote = 'requested run'
}

$runPath = if ([string]::IsNullOrWhiteSpace($RunId)) { $null } else { Join-Path $runsRoot $RunId }
$workersPath = if ($runPath) { Join-Path $runPath 'workers' } else { $null }
$eventsPath = if ($runPath) { Join-Path $runPath 'events.jsonl' } else { $null }

if ($runPath -and -not (Test-Path -LiteralPath $runPath -PathType Container)) {
  Write-Warning "[loopcoder] run state not found: $runPath; local state will be empty"
}

$attempts = @(Read-LocalAttempts -WorkersPath $workersPath)
$eventCount = Read-EventCount -EventsPath $eventsPath

$issueMap = @{}
foreach ($issue in $openIssues) {
  $labels = Get-LabelNames -Issue $issue
  $stateReason = [string](Get-JsonProperty -Object $issue -Name 'stateReason' -Default '')
  $issueMap[[string]$issue.number] = New-IssueRecord -Number ([int]$issue.number) -Title ([string]$issue.title) -State 'OPEN' -StateReason $stateReason -Labels $labels
}

$candidateIssueNumbers = @()
$candidateIssueNumbers += @($openIssues | ForEach-Object { [int]$_.number })
$candidateIssueNumbers += @($attempts | ForEach-Object { [int]$_.issue })
$candidateIssueNumbers += @($openPrs | ForEach-Object { $_.issue_numbers } | ForEach-Object { [int]$_ })
$candidateIssueNumbers = @($candidateIssueNumbers | Where-Object { $_ -gt 0 } | Select-Object -Unique | Sort-Object)

foreach ($number in $candidateIssueNumbers) {
  if ($issueMap.ContainsKey([string]$number)) { continue }
  $issueView = Invoke-Gh -GhArgs @('issue', 'view', [string]$number, '--json', 'number,title,state,stateReason,labels,closedByPullRequestsReferences')
  $issueDetails = @(Convert-GhJson -Result $issueView -Description "issue view $number" -AllowFailure)
  if ($issueDetails.Count -gt 0) {
    $detail = $issueDetails[0]
    $labels = Get-LabelNames -Issue $detail
    $stateReason = [string](Get-JsonProperty -Object $detail -Name 'stateReason' -Default '')
    $closedRefs = @(Get-JsonProperty -Object $detail -Name 'closedByPullRequestsReferences' -Default @())
    $issueMap[[string]$number] = New-IssueRecord -Number ([int]$detail.number) -Title ([string]$detail.title) -State ([string]$detail.state) -StateReason $stateReason -Labels $labels -ClosedByPullRequestsReferences $closedRefs
  } else {
    $issueMap[[string]$number] = New-IssueRecord -Number $number -Title '(issue details unavailable)' -State 'UNKNOWN' -Labels @()
  }
}

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

$reports = @()
foreach ($number in $candidateIssueNumbers) {
  $key = [string]$number
  $issue = $issueMap[$key]
  $issueAttempts = if ($attemptsByIssue.ContainsKey($key)) { @($attemptsByIssue[$key]) } else { @() }
  $latestAttempt = $issueAttempts | Sort-Object attempt, last_write_utc | Select-Object -Last 1
  $candidateBranches = Get-CandidateBranches -IssueNumber $number -Attempts $issueAttempts

  $matchingPrs = @()
  if ($prsByIssue.ContainsKey($key)) { $matchingPrs += @($prsByIssue[$key]) }
  foreach ($branch in $candidateBranches) {
    if ($prsByBranch.ContainsKey($branch)) { $matchingPrs += $prsByBranch[$branch] }
  }
  $matchingPrs = @($matchingPrs | Sort-Object number -Unique)
  $primaryPr = $matchingPrs | Select-Object -First 1

  $classification = 'needs-inspection'
  $actionKind = 'blocked'
  $action = 'manual inspection needed before dispatching anything'
  $pidEvidence = 'pid: n/a'
  $attemptEvidence = 'attempt: none'
  $branchEvidence = 'branch: n/a'
  $heartbeatAge = $null
  $progressAge = $null
  $pidState = $null
  $remoteBranchState = $null

  if ($latestAttempt) {
    $attemptEvidence = "attempt: job=$($latestAttempt.job_id), attempt=$($latestAttempt.attempt), status=$($latestAttempt.status), phase=$($latestAttempt.phase), sidecar=$($latestAttempt.path)"
    $remoteBranchState = Test-RemoteBranch -Branch $latestAttempt.branch
    $branchEvidence = $remoteBranchState.evidence
  }

  $closingPrRefs = @(Get-ClosingPrRefs -Issue $issue)
  $stateReason = [string](Get-JsonProperty -Object $issue -Name 'stateReason' -Default '')
  $closedAsCompleted = ($issue.state -eq 'CLOSED' -and $stateReason -eq 'COMPLETED')
  if ($issue.state -eq 'CLOSED' -and ($closedAsCompleted -or $closingPrRefs.Count -gt 0)) {
    $classification = 'done'
    $actionKind = 'none'
    $action = 'done on GitHub; no local recovery needed'
  } elseif ($primaryPr) {
    if ($latestAttempt -and $latestAttempt.status -eq 'running' -and $candidateBranches -contains $primaryPr.headRefName) {
      $classification = 'adopt-PR'
      $action = "adopt open PR #$($primaryPr.number); do not dispatch a duplicate worker"
    } else {
      $classification = Get-PrClassification -Pr $primaryPr
      $action = "open PR #$($primaryPr.number) exists; do not dispatch a duplicate worker"
    }
    $actionKind = 'blocked'
  } elseif ($latestAttempt) {
    $heartbeatAge = Get-AgeSeconds -Timestamp $latestAttempt.heartbeat_at
    $progressAge = Get-AgeSeconds -Timestamp $latestAttempt.last_progress_at
    $pidState = Test-PidLiveness -PidValue $latestAttempt.pid

    $heartbeatFresh = ($null -ne $heartbeatAge -and $heartbeatAge -le $heartbeatFreshSeconds)
    $progressStale = ($null -ne $progressAge -and $progressAge -gt [int]$thresholds.stale_after_seconds)
    $progressHung = ($null -ne $progressAge -and $progressAge -gt [int]$thresholds.hung_after_seconds)
    $heartbeatStale = ($null -ne $heartbeatAge -and $heartbeatAge -gt $heartbeatFreshSeconds)

    if ($heartbeatFresh -and $pidState.alive -and -not $progressStale) {
      $classification = 'running'
      $actionKind = 'blocked'
      $action = 'live local attempt exists; do not dispatch'
    } elseif ($heartbeatFresh -and $progressHung) {
      $classification = 'hung'
      $actionKind = if ($pidState.alive) { 'blocked' } else { 'ready' }
      $action = if ($pidState.alive) { 'progress is hung but pid is still alive; inspect before recovery' } else { 'recover the hung attempt; no open PR or live pid found' }
    } elseif ($heartbeatFresh -and $progressStale) {
      $classification = 'stale'
      $actionKind = 'blocked'
      $action = 'progress is stale; inspect before deciding to recover'
    } elseif ($heartbeatStale -and -not $pidState.alive) {
      $classification = 'orphaned'
      $actionKind = 'ready'
      $action = 'recover or re-dispatch from the local recovery context; no open PR or live pid found'
    } elseif ($progressHung) {
      $classification = 'hung'
      $actionKind = if ($pidState.alive) { 'blocked' } else { 'ready' }
      $action = if ($pidState.alive) { 'progress is hung and pid is alive; inspect before recovery' } else { 'recover the hung attempt; no open PR or live pid found' }
    } elseif ($progressStale) {
      $classification = 'stale'
      $actionKind = 'blocked'
      $action = 'progress is stale; inspect before deciding to recover'
    } else {
      $classification = 'needs-inspection'
      $actionKind = 'blocked'
      $action = 'attempt or branch exists without an open PR; inspect before dispatching'
    }

    $pidEvidence = $pidState.evidence
  } else {
    $blockedLabels = Get-BlockedLabels -Labels (Get-LabelNames -Issue $issue)
    if ($blockedLabels.Count -gt 0) {
      $classification = 'needs-inspection'
      $actionKind = 'blocked'
      $action = "blocked by labels: $($blockedLabels -join ', ')"
    } elseif ($issue.state -eq 'OPEN') {
      $classification = 'ready'
      $actionKind = 'ready'
      $action = 'ready to dispatch; no open PR or live local attempt found'
    } else {
      $classification = 'needs-inspection'
      $actionKind = 'blocked'
      $action = "issue state is $($issue.state) without completed state reason or closing PR reference"
    }
  }

  $labels = Get-LabelNames -Issue $issue
  $prEvidence = if ($primaryPr) {
    "PR: #$($primaryPr.number) $($primaryPr.headRefName), checks=$($primaryPr.check_summary.text)"
  } elseif ($matchingPrs.Count -gt 0) {
    "PR: $($matchingPrs.Count) matching open PRs"
  } else {
    'PR: none open'
  }
  $closureEvidence = if ($closingPrRefs.Count -gt 0) {
    'closing PRs: ' + (($closingPrRefs | ForEach-Object {
      "#$((Get-JsonProperty -Object $_ -Name 'number' -Default '?'))"
    }) -join ', ')
  } elseif ($classification -eq 'done' -and $closedAsCompleted) {
    'closing PRs: closed as completed'
  } else {
    'closing PRs: none observed'
  }

  $reports += [pscustomobject]@{
    number = $number
    title = $issue.title
    state = $issue.state
    labels = $labels
    classification = $classification
    action_kind = $actionKind
    action = $action
    pr = $primaryPr
    attempts = $issueAttempts
    latest_attempt = $latestAttempt
    heartbeat_age = $heartbeatAge
    progress_age = $progressAge
    evidence = [ordered]@{
      pr = $prEvidence
      attempt = $attemptEvidence
      pid = $pidEvidence
      branch = $branchEvidence
      heartbeat = "heartbeat age=$(Format-Age -AgeSeconds $heartbeatAge), progress age=$(Format-Age -AgeSeconds $progressAge)"
      closure = $closureEvidence
    }
  }
}

Write-Host 'RESUME REPORT'
Write-Host "Repo: $Repo"
Write-Host "Base branch: $BaseBranch"
Write-Host "RunId: $(if ([string]::IsNullOrWhiteSpace($RunId)) { '(none)' } else { $RunId }) ($selectedRunNote)"
Write-Host "Generated at: $(Get-UtcIso)"
Write-Host "GitHub snapshot: open issues=$($openIssues.Count), open PRs=$($openPrs.Count)"
Write-Host "Local state: attempts=$($attempts.Count), events=$eventCount"
Write-Host "Thresholds: heartbeat fresh <= ${heartbeatFreshSeconds}s, stale progress > $($thresholds.stale_after_seconds)s, hung progress > $($thresholds.hung_after_seconds)s"
Write-Host ''
Write-Host 'Issues'

foreach ($report in ($reports | Sort-Object number)) {
  $labelText = if ($report.labels.Count -gt 0) { $report.labels -join ', ' } else { '(none)' }
  Write-Host "- #$($report.number) $($report.title)"
  Write-Host "  state: $($report.state); labels: $labelText"
  Write-Host "  classification: $($report.classification)"
  Write-Host "  evidence: $($report.evidence.pr)"
  Write-Host "  evidence: $($report.evidence.attempt)"
  Write-Host "  evidence: $($report.evidence.pid)"
  Write-Host "  evidence: $($report.evidence.branch)"
  Write-Host "  evidence: $($report.evidence.heartbeat)"
  Write-Host "  evidence: $($report.evidence.closure)"
  Write-Host "  next: $($report.action)"
}

Write-Host ''
Write-Host 'Next ready actions'
$readyReports = @($reports | Where-Object { $_.action_kind -eq 'ready' } | Sort-Object number)
if ($readyReports.Count -eq 0) {
  Write-Host '- none'
} else {
  foreach ($report in $readyReports) {
    Write-Host "- #$($report.number): $($report.action) (classification=$($report.classification))"
  }
}

Write-Host ''
Write-Host 'Blocked / awaiting human input'
$blockedReports = @($reports | Where-Object { $_.action_kind -eq 'blocked' } | Sort-Object number)
if ($blockedReports.Count -eq 0) {
  Write-Host '- none'
} else {
  foreach ($report in $blockedReports) {
    Write-Host "- #$($report.number): $($report.action) (classification=$($report.classification))"
  }
}

Write-Host ''
Write-Host 'Safety'
Write-Host '- resume is read-mostly: no dispatch, no merge, no push, and no GitHub mutation was attempted.'
