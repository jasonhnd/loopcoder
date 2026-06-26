#Requires -Version 7
<#
.SYNOPSIS
  Run configured local verification command gates for a PR or branch.
#>
[CmdletBinding(DefaultParameterSetName = 'Pr')]
param(
  [Parameter(Mandatory)][string]$Repo,
  [Parameter(Mandatory, ParameterSetName = 'Pr')][int]$PrNumber,
  [Parameter(Mandatory, ParameterSetName = 'Branch')][string]$Branch,
  [string]$BaseBranch = 'main'
)
$ErrorActionPreference = 'Stop'

$script:resolvedRepo = $null
$script:scratchPath = $null
$script:worktreePath = $null
$script:cleanupNotes = @()

function Get-UtcIso { (Get-Date).ToUniversalTime().ToString('o') }

function Get-Indent {
  param([string]$Line)
  if ($Line -match '^(\s*)') { return $Matches[1].Length }
  return 0
}

function Remove-YamlComment {
  param([AllowNull()][string]$Line)
  if ($null -eq $Line) { return '' }

  $inSingle = $false
  $inDouble = $false
  for ($i = 0; $i -lt $Line.Length; $i++) {
    $ch = $Line[$i]
    if ($ch -eq "'" -and -not $inDouble) {
      if ($inSingle -and ($i + 1) -lt $Line.Length -and $Line[$i + 1] -eq "'") {
        $i++
        continue
      }
      $inSingle = -not $inSingle
      continue
    }
    if ($ch -eq '"' -and -not $inSingle) {
      $escaped = ($i -gt 0 -and $Line[$i - 1] -eq '\')
      if (-not $escaped) { $inDouble = -not $inDouble }
      continue
    }
    if ($ch -eq '#' -and -not $inSingle -and -not $inDouble) {
      if ($i -eq 0 -or [char]::IsWhiteSpace($Line[$i - 1])) {
        return $Line.Substring(0, $i)
      }
    }
  }
  return $Line
}

function ConvertFrom-YamlScalar {
  param([AllowNull()][string]$Value)
  if ($null -eq $Value) { return '' }

  $trimmed = $Value.Trim()
  if ($trimmed.Length -ge 2 -and $trimmed.StartsWith("'") -and $trimmed.EndsWith("'")) {
    return $trimmed.Substring(1, $trimmed.Length - 2).Replace("''", "'")
  }
  if ($trimmed.Length -ge 2 -and $trimmed.StartsWith('"') -and $trimmed.EndsWith('"')) {
    return $trimmed.Substring(1, $trimmed.Length - 2).Replace('\"', '"')
  }
  return $trimmed
}

function Split-InlineYamlList {
  param([string]$Value)

  $trimmed = (Remove-YamlComment $Value).Trim()
  if (-not $trimmed.StartsWith('[')) { return @() }
  $end = $trimmed.LastIndexOf(']')
  if ($end -lt 0) { return @() }
  $content = $trimmed.Substring(1, $end - 1)
  if ([string]::IsNullOrWhiteSpace($content)) { return @() }

  $parts = @()
  $start = 0
  $inSingle = $false
  $inDouble = $false
  for ($i = 0; $i -lt $content.Length; $i++) {
    $ch = $content[$i]
    if ($ch -eq "'" -and -not $inDouble) {
      if ($inSingle -and ($i + 1) -lt $content.Length -and $content[$i + 1] -eq "'") {
        $i++
        continue
      }
      $inSingle = -not $inSingle
      continue
    }
    if ($ch -eq '"' -and -not $inSingle) {
      $escaped = ($i -gt 0 -and $content[$i - 1] -eq '\')
      if (-not $escaped) { $inDouble = -not $inDouble }
      continue
    }
    if ($ch -eq ',' -and -not $inSingle -and -not $inDouble) {
      $parts += $content.Substring($start, $i - $start)
      $start = $i + 1
    }
  }
  $parts += $content.Substring($start)

  return @($parts |
    ForEach-Object { ConvertFrom-YamlScalar $_ } |
    Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Read-YamlBlockList {
  param(
    [string[]]$Lines,
    [int]$StartIndex,
    [int]$ParentIndent
  )

  $items = @()
  for ($i = $StartIndex; $i -lt $Lines.Count; $i++) {
    $rawLine = $Lines[$i]
    $withoutComment = Remove-YamlComment $rawLine
    if ([string]::IsNullOrWhiteSpace($withoutComment)) { continue }

    $indent = Get-Indent $rawLine
    if ($indent -le $ParentIndent) { break }

    if ($withoutComment -match '^\s*-\s*(.*)$') {
      $value = ConvertFrom-YamlScalar $Matches[1]
      if (-not [string]::IsNullOrWhiteSpace($value)) { $items += $value }
    }
  }
  return @($items)
}

function Read-LocalCommandGroups {
  param([string]$RepoPath)

  $groups = [ordered]@{
    tests = @()
    typecheck = @()
    build = @()
  }
  $configPath = Join-Path $RepoPath '.delivery.yml'
  if (-not (Test-Path -LiteralPath $configPath -PathType Leaf)) {
    return $groups
  }

  $lines = @(Get-Content -LiteralPath $configPath -ErrorAction Stop)
  $inCi = $false
  $ciIndent = -1
  for ($i = 0; $i -lt $lines.Count; $i++) {
    $rawLine = $lines[$i]
    $line = Remove-YamlComment $rawLine
    if ([string]::IsNullOrWhiteSpace($line)) { continue }

    $indent = Get-Indent $rawLine
    if (-not $inCi) {
      if ($line -match '^\s*ci\s*:\s*$') {
        $inCi = $true
        $ciIndent = $indent
      }
      continue
    }

    if ($indent -le $ciIndent) { break }
    if ($line -match '^\s*(tests|typecheck|build)\s*:\s*(.*)$') {
      $groupName = $Matches[1]
      $value = (Remove-YamlComment $Matches[2]).Trim()
      if ([string]::IsNullOrWhiteSpace($value) -or $value -eq '[]' -or $value -match '^(~|null)$') {
        if ([string]::IsNullOrWhiteSpace($value)) {
          $groups[$groupName] = @(Read-YamlBlockList -Lines $lines -StartIndex ($i + 1) -ParentIndent $indent)
        } else {
          $groups[$groupName] = @()
        }
      } elseif ($value.StartsWith('[')) {
        $groups[$groupName] = @(Split-InlineYamlList -Value $value)
      } else {
        $groups[$groupName] = @(ConvertFrom-YamlScalar $value)
      }
    }
  }

  return $groups
}

function Invoke-External {
  param(
    [Parameter(Mandatory)][string]$File,
    [string[]]$Arguments = @(),
    [string]$WorkingDirectory
  )

  $previous = $null
  try {
    if (-not [string]::IsNullOrWhiteSpace($WorkingDirectory)) {
      $previous = (Get-Location).Path
      Set-Location -LiteralPath $WorkingDirectory
    }

    $output = @(& $File @Arguments 2>&1 | ForEach-Object { [string]$_ })
    $exitCode = if ($null -ne $LASTEXITCODE) { [int]$LASTEXITCODE } else { 0 }
    return [pscustomobject]@{
      exit_code = $exitCode
      output = $output
    }
  } catch {
    return [pscustomobject]@{
      exit_code = 127
      output = @($_.Exception.Message)
    }
  } finally {
    if ($previous) { Set-Location -LiteralPath $previous }
  }
}

function Get-BoundedLogTail {
  param(
    [object[]]$Lines,
    [int]$MaxLines = 80,
    [int]$MaxChars = 12000,
    [int]$MaxLineChars = 500
  )

  $selected = @($Lines | ForEach-Object {
    $line = [string]$_
    if ($line.Length -gt $MaxLineChars) {
      $line.Substring(0, $MaxLineChars) + '...'
    } else {
      $line
    }
  } | Select-Object -Last $MaxLines)

  while ($selected.Count -gt 1 -and (($selected -join [Environment]::NewLine).Length -gt $MaxChars)) {
    $selected = @($selected | Select-Object -Skip 1)
  }
  return @($selected)
}

function Get-FailureClassification {
  param(
    [string]$Command,
    [int]$ExitCode,
    [object[]]$Output
  )

  if ($ExitCode -eq 0) {
    return [pscustomobject]@{ status = 'pass'; reason = '' }
  }

  $text = ($Output -join [Environment]::NewLine)
  $missingToolPatterns = @(
    'CommandNotFoundException',
    "The term '.+' is not recognized",
    'is not recognized as (an internal|the name)',
    'command not found',
    'executable file not found',
    'No such file or directory.*(npm|pnpm|yarn|bun|node|python|pip|cargo|go|dotnet|pwsh|bash)'
  )
  foreach ($pattern in $missingToolPatterns) {
    if ($text -match $pattern) {
      return [pscustomobject]@{ status = 'needs-human'; reason = 'missing-tool' }
    }
  }

  $dependencyInstallCommands = @(
    '^\s*(npm|pnpm|yarn|bun)\s+(ci|install)\b',
    '^\s*(pip|pip3)\s+install\b',
    '^\s*poetry\s+install\b',
    '^\s*cargo\s+fetch\b',
    '^\s*go\s+mod\s+download\b',
    '^\s*dotnet\s+restore\b'
  )
  foreach ($pattern in $dependencyInstallCommands) {
    if ($Command -match $pattern) {
      return [pscustomobject]@{ status = 'needs-human'; reason = 'dependency-install-failure' }
    }
  }

  $dependencyFailurePatterns = @(
    '\b(EAI_AGAIN|ENOTFOUND|ECONNRESET|ETIMEDOUT|CERT_HAS_EXPIRED|SELF_SIGNED_CERT)\b',
    'unable to verify the first certificate',
    'Could not resolve host',
    'failed to (fetch|download|resolve)',
    'network.*(error|unreachable|timeout)',
    'install.*failed'
  )
  foreach ($pattern in $dependencyFailurePatterns) {
    if ($text -match $pattern) {
      return [pscustomobject]@{ status = 'needs-human'; reason = 'dependency-install-failure' }
    }
  }

  return [pscustomobject]@{ status = 'fail'; reason = 'command-exit-nonzero' }
}

function New-IsolatedCheckout {
  param(
    [string]$RepoPath,
    [string]$Base,
    [AllowNull()][object]$PullRequestNumber,
    [AllowNull()][string]$HeadBranch
  )

  $script:scratchPath = Join-Path ([IO.Path]::GetTempPath()) ("loopcoder-verify-local-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
  $script:worktreePath = Join-Path $script:scratchPath 'wt'
  New-Item -ItemType Directory -Force -Path $script:scratchPath | Out-Null

  $baseFetch = Invoke-External -File 'git' -Arguments @('-C', $RepoPath, 'fetch', '-q', 'origin', $Base)
  if ($baseFetch.exit_code -ne 0) {
    throw "could not fetch base branch '$Base': $($baseFetch.output -join [Environment]::NewLine)"
  }

  $add = Invoke-External -File 'git' -Arguments @('-C', $RepoPath, 'worktree', 'add', '--detach', $script:worktreePath, "origin/$Base")
  if ($add.exit_code -ne 0) {
    throw "could not create isolated worktree: $($add.output -join [Environment]::NewLine)"
  }

  if ($null -ne $PullRequestNumber) {
    $checkout = Invoke-External -File 'gh' -Arguments @('pr', 'checkout', [string]$PullRequestNumber, '--detach') -WorkingDirectory $script:worktreePath
    if ($checkout.exit_code -ne 0) {
      throw "could not check out PR #${PullRequestNumber}: $($checkout.output -join [Environment]::NewLine)"
    }
  } else {
    $remoteBranch = $HeadBranch
    if ($remoteBranch.StartsWith('origin/')) {
      $remoteBranch = $remoteBranch.Substring('origin/'.Length)
    }

    $fetchBranch = Invoke-External -File 'git' -Arguments @('-C', $script:worktreePath, 'fetch', '-q', 'origin', $remoteBranch)
    if ($fetchBranch.exit_code -eq 0) {
      $checkoutBranch = Invoke-External -File 'git' -Arguments @('-C', $script:worktreePath, 'checkout', '--detach', 'FETCH_HEAD')
      if ($checkoutBranch.exit_code -ne 0) {
        throw "could not check out fetched branch '$HeadBranch': $($checkoutBranch.output -join [Environment]::NewLine)"
      }
    } else {
      $rev = Invoke-External -File 'git' -Arguments @('-C', $RepoPath, 'rev-parse', '--verify', "$HeadBranch^{commit}")
      if ($rev.exit_code -ne 0 -or $rev.output.Count -eq 0) {
        throw "could not resolve branch '$HeadBranch': $($fetchBranch.output -join [Environment]::NewLine)"
      }
      $commit = ([string]$rev.output[0]).Trim()
      $checkoutLocal = Invoke-External -File 'git' -Arguments @('-C', $script:worktreePath, 'checkout', '--detach', $commit)
      if ($checkoutLocal.exit_code -ne 0) {
        throw "could not check out local branch '$HeadBranch': $($checkoutLocal.output -join [Environment]::NewLine)"
      }
    }
  }

  return $script:worktreePath
}

function Invoke-CommandGroup {
  param(
    [string]$Name,
    [string[]]$Commands,
    [string]$WorktreePath
  )

  $commandResults = @()
  foreach ($command in $Commands) {
    Write-Host "[loopcoder] running $Name gate: $command"
    $result = Invoke-External -File 'pwsh' -Arguments @('-NoLogo', '-NoProfile', '-NonInteractive', '-Command', $command) -WorkingDirectory $WorktreePath
    $classification = Get-FailureClassification -Command $command -ExitCode $result.exit_code -Output $result.output

    $commandResults += [ordered]@{
      command = $command
      exit_code = $result.exit_code
      status = $classification.status
      reason = $classification.reason
      log_tail = @(Get-BoundedLogTail -Lines $result.output)
    }
  }

  $statuses = @($commandResults | ForEach-Object { $_['status'] })
  $groupStatus = 'pass'
  if ($statuses -contains 'needs-human') {
    $groupStatus = 'needs-human'
  } elseif ($statuses -contains 'fail') {
    $groupStatus = 'fail'
  }

  return [ordered]@{
    group = $Name
    status = $groupStatus
    commands = @($commandResults)
  }
}

function Remove-IsolatedCheckout {
  if ($script:worktreePath -and $script:resolvedRepo) {
    try {
      $remove = Invoke-External -File 'git' -Arguments @('-C', $script:resolvedRepo, 'worktree', 'remove', $script:worktreePath, '--force')
      if ($remove.exit_code -eq 0) {
        $script:cleanupNotes += "removed worktree $script:worktreePath"
      } else {
        $script:cleanupNotes += "worktree cleanup failed: $($remove.output -join ' ')"
      }
    } catch {
      $script:cleanupNotes += "worktree cleanup threw: $($_.Exception.Message)"
    }
  }

  if ($script:scratchPath -and (Test-Path -LiteralPath $script:scratchPath)) {
    try {
      Remove-Item -LiteralPath $script:scratchPath -Recurse -Force -ErrorAction Stop
      $script:cleanupNotes += "removed scratch $script:scratchPath"
    } catch {
      $script:cleanupNotes += "scratch cleanup failed: $($_.Exception.Message)"
    }
  }
}

function Write-StructuredSummary {
  param([System.Collections.IDictionary]$Summary)

  Write-Host 'LOCAL VERIFICATION SUMMARY'
  Write-Host "verdict: $($Summary['verdict'])"
  Write-Host "local_command_gates: $($Summary['local_command_gates'])"
  if ($Summary.Contains('groups')) {
    foreach ($group in @($Summary['groups'])) {
      Write-Host "- $($group['group']): $($group['status'])"
      foreach ($command in @($group['commands'])) {
        Write-Host "  - $($command['status']) exit=$($command['exit_code']) command=$($command['command'])"
      }
    }
  }
  Write-Host ''
  Write-Host 'JSON SUMMARY'
  $Summary | ConvertTo-Json -Depth 12
}

$summary = $null
$exitCode = 2

try {
  $script:resolvedRepo = (Resolve-Path -LiteralPath $Repo).Path
  $groups = Read-LocalCommandGroups -RepoPath $script:resolvedRepo
  $configuredGroups = @()
  foreach ($name in @('tests', 'typecheck', 'build')) {
    $commands = @($groups[$name])
    if ($commands.Count -gt 0) {
      $configuredGroups += [ordered]@{
        name = $name
        commands = $commands
      }
    }
  }

  if ($configuredGroups.Count -eq 0) {
    Write-Host 'no local command gates configured'
    $summary = [ordered]@{
      repo = $script:resolvedRepo
      pr = if ($PSCmdlet.ParameterSetName -eq 'Pr') { $PrNumber } else { $null }
      branch = if ($PSCmdlet.ParameterSetName -eq 'Branch') { $Branch } else { $null }
      base_branch = $BaseBranch
      generated_at = Get-UtcIso
      local_command_gates = 'not-configured'
      verdict = 'pass'
      groups = @()
    }
    $exitCode = 0
  } else {
    $pullRequestNumber = if ($PSCmdlet.ParameterSetName -eq 'Pr') { $PrNumber } else { $null }
    $headBranch = if ($PSCmdlet.ParameterSetName -eq 'Branch') { $Branch } else { $null }
    $worktree = New-IsolatedCheckout -RepoPath $script:resolvedRepo -Base $BaseBranch -PullRequestNumber $pullRequestNumber -HeadBranch $headBranch

    $groupResults = @()
    foreach ($group in $configuredGroups) {
      $groupResults += Invoke-CommandGroup -Name $group['name'] -Commands $group['commands'] -WorktreePath $worktree
    }

    $groupStatuses = @($groupResults | ForEach-Object { $_['status'] })
    $verdict = 'pass'
    $exitCode = 0
    if ($groupStatuses -contains 'needs-human') {
      $verdict = 'needs-human'
      $exitCode = 2
    } elseif ($groupStatuses -contains 'fail') {
      $verdict = 'fail'
      $exitCode = 1
    }

    $summary = [ordered]@{
      repo = $script:resolvedRepo
      worktree = $worktree
      pr = if ($PSCmdlet.ParameterSetName -eq 'Pr') { $PrNumber } else { $null }
      branch = if ($PSCmdlet.ParameterSetName -eq 'Branch') { $Branch } else { $null }
      base_branch = $BaseBranch
      generated_at = Get-UtcIso
      local_command_gates = 'configured'
      verdict = $verdict
      groups = @($groupResults)
    }
  }
} catch {
  $summary = [ordered]@{
    repo = if ($script:resolvedRepo) { $script:resolvedRepo } else { $Repo }
    pr = if ($PSCmdlet.ParameterSetName -eq 'Pr') { $PrNumber } else { $null }
    branch = if ($PSCmdlet.ParameterSetName -eq 'Branch') { $Branch } else { $null }
    base_branch = $BaseBranch
    generated_at = Get-UtcIso
    local_command_gates = 'error'
    verdict = 'needs-human'
    error = $_.Exception.Message
    groups = @()
  }
  $exitCode = 2
} finally {
  Remove-IsolatedCheckout
}

if ($summary) {
  $summary['cleanup'] = @($script:cleanupNotes)
  Write-StructuredSummary -Summary $summary
}
exit $exitCode
