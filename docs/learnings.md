# loopcoder Learnings

Status: APPEND ONLY. Entries may be superseded by later entries, but existing
entries are not rewritten except to fix formatting or remove sensitive data.

This file has advisory authority only. It informs conductor and worker prompts,
but it never overrides:

1. System and tool safety constraints.
2. The user's current request.
3. [PROCESS.md](PROCESS.md).
4. [`../SKILL.md`](../SKILL.md).
5. `.delivery.yml` in the target repository.
6. Issue acceptance criteria.

If a learning conflicts with a higher-authority source, prefer the
higher-authority source and report the conflict.

## Entry Template

### YYYY-MM-DD - run <run-id> - <short title>

- Scope: <issue, PR, or command>
- Role: conductor | worker | verifier | human
- Observed: <what happened>
- Evidence: <links to issue, PR, check, log, or command output>
- Learning: <reusable fact or pattern>
- Applies to: <SKILL.md | scripts | docs | scheduling | worker prompts | repo-specific>
- Candidate improvement: <none | suggested issue title>
- Confidence: low | medium | high
- Supersedes: <optional earlier entry id>
