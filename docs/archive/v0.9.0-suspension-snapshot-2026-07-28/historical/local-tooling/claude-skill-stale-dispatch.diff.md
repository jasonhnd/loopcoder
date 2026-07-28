```diff
diff --git a/SKILL.md b/local-claude-skill/SKILL.md
index 780bdc0..b516287 100644
--- a/SKILL.md
+++ b/local-claude-skill/SKILL.md
@@ -79,10 +79,10 @@ Per [`docs/specs/0316-conductor-local-enforcement.md`](docs/specs/0316-conductor
 and [`docs/specs/0447-relay-enforcement-hardgate.md`](docs/specs/0447-relay-enforcement-hardgate.md),
 Worker and Verifier report relay is a hard local visible-output
 obligation. Never swallow the report block: do not redirect, hide, or
-suppress stderr from foreground `loopcoder dispatch` or `loopcoder loopreview`,
-and keep `loopcoder dispatch-wave --foreground` stdout visible because each
-Worker pretty block streams there as that Worker completes. Each Worker and
-Verifier pretty report block must be relayed verbatim, never summarized, merged,
+suppress stderr from `loopcoder dispatch` or `loopcoder loopreview`, and keep
+foreground `loopcoder dispatch-wave` stdout visible because each Worker pretty
+block streams there as that Worker completes. Each Worker and Verifier pretty
+report block must be relayed verbatim, never summarized, merged,
 rewrapped, or hand-reformatted.
 
 The Go binary enforces a cross-command relay gate for mechanical progress. When
@@ -274,7 +274,7 @@ Bounds and scrutiny:
    - Compute the ready set with `loopcoder ready-set --repo . --base-branch <base-branch> --run-id <run-id> --format text`.
    - A ready issue is an unstarted issue whose `depends_on` entries / `blocked-by:#N` labels are all merged to `main`, not merely open as PRs.
    - Keep the two ordering axes from [`docs/specs/0028-scheduling.md`](docs/specs/0028-scheduling.md) separate: a real code dependency forces serial order, so B waits until A is merged and then branches from `main`; file overlap does not block dispatch.
-   - Dispatch one ready wave with `loopcoder dispatch-wave --repo . --base-branch <base-branch> --run-id <run-id> ...`, or dispatch one issue with `loopcoder dispatch ...`. Host-profiled non-interactive launches detach by default and return status/attach/cancel commands; pass `--foreground` only when the conductor intentionally waits and relays streamed receipts in the current turn. Then recompute as PRs merge. Repeat until the DAG is drained or blocked.
+   - Dispatch one ready wave with `loopcoder dispatch-wave --repo . --base-branch <base-branch> --run-id <run-id> ...`, or dispatch one issue with `loopcoder dispatch ...`. Then recompute as PRs merge. Repeat until the DAG is drained or blocked.
    - `loopcoder dispatch` and `loopcoder dispatch-wave` preserve git worktree creation serialization, so independent ready issues can be dispatched concurrently safely.
    - Call the selected backend once per ready issue or ready wave. Do not recreate worktree, worker-agent invocation, commit, push, or PR logic in the conductor.
    - Invoke `loopcoder dispatch` and `loopcoder dispatch-wave` with
@@ -302,10 +302,9 @@ Bounds and scrutiny:
      --issue-body "<body>" \
      --base-branch <base-branch> \
      --provider <worker-provider> \
-     --pretty \
-     --foreground
+     --pretty
 
-   loopcoder dispatch-wave --repo . --base-branch <base-branch> --run-id <run-id> --issue-numbers <n1>,<n2> --pretty --foreground
+   loopcoder dispatch-wave --repo . --base-branch <base-branch> --run-id <run-id> --issue-numbers <n1>,<n2> --pretty
    ```
 
 5. Verify each resulting PR.
```
