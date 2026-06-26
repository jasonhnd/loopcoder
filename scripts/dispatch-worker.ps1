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
  [ValidateSet('codex')][string]$Provider = 'codex',   # v1: codex only (Worker port is provider-pluggable)
  [string]$Model,
  [string]$Effort,
  [switch]$KeepWorktree
)
$ErrorActionPreference = 'Stop'
function Log($m){ Write-Host "[loopcoder] $m" }
function Quote-CmdArg($arg){ '"' + ($arg -replace '"','\"') + '"' }

$Repo = (Resolve-Path -LiteralPath $Repo).Path
if (-not $Branch) { $Branch = "loop/issue-$IssueNumber" }

Push-Location $Repo
try { $slug = (& gh repo view --json nameWithOwner -q .nameWithOwner) } finally { Pop-Location }
if (-not $slug) { throw "could not resolve GitHub repo (gh repo view) — need a repo with a GitHub remote." }

$scratch     = Join-Path ([IO.Path]::GetTempPath()) ("loopcoder-" + [guid]::NewGuid().ToString('N').Substring(0,8))
New-Item -ItemType Directory -Force -Path $scratch | Out-Null
$wt          = Join-Path $scratch 'wt'
$promptFile  = Join-Path $scratch 'prompt.txt'
$summaryFile = Join-Path $scratch 'summary.txt'
$logFile     = Join-Path $scratch 'codex.log'

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

try {
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
  Set-Content -LiteralPath $promptFile -Value $prompt -Encoding utf8

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
  cmd /c "$codexCommand < `"$promptFile`" > `"$logFile`" 2>&1"
  if ($LASTEXITCODE -ne 0) { throw "codex exec failed (exit $LASTEXITCODE). See $logFile" }
  $summary = if (Test-Path $summaryFile) { (Get-Content -LiteralPath $summaryFile -Raw).Trim() } else { '(codex produced no summary)' }

  $dirty = git -C $wt status --porcelain
  if (-not $dirty) { throw "codex made no file changes for issue #$IssueNumber (nothing to commit)" }

  Log "commit + push"
  git -C $wt add -A
  git -C $wt commit -q -m "$IssueTitle (closes #$IssueNumber)"
  if ($LASTEXITCODE -ne 0) { throw "git commit failed" }
  git -C $wt push -q -u origin $Branch
  if ($LASTEXITCODE -ne 0) { throw "git push failed" }

  Log "open PR"
  $body  = "Closes #$IssueNumber`n`n$summary`n`n— opened by loopcoder (worker: $Provider)"
  $prUrl = & gh pr create -R $slug --head $Branch --base $BaseBranch --title $IssueTitle --body $body
  if ($LASTEXITCODE -ne 0) { throw "gh pr create failed" }

  Log "done: $prUrl"
  [pscustomobject]@{ ok = $true; issue = $IssueNumber; branch = $Branch; pr = "$prUrl"; summary = $summary } | ConvertTo-Json -Compress
}
finally {
  if (-not $KeepWorktree) {
    git -C $Repo worktree remove $wt --force 2>$null
    git -C $Repo branch -D $Branch 2>$null         # local branch only; pushed copy stays on origin for the PR
    Remove-Item -Recurse -Force $scratch -ErrorAction SilentlyContinue
  } else {
    Log "kept worktree: $wt   (scratch: $scratch)"
  }
}
