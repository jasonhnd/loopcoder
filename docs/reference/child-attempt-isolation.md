# Cross-provider child-attempt isolation (V090-063)

Package: [`internal/childattempt`](../../internal/childattempt)  
Issue: [#1175](https://github.com/jasonhnd/loopcoder/issues/1175)

## Purpose

Run an explicitly materialized WorkItem on a different provider as its own
Attempt with independent claim, route, worktree, credentials, and terminal
evidence.

## Isolation

- No shared writable worktree or credential scope  
- Sibling private outputs hidden by default  
- Parent cannot rewrite child terminals  
- Aggregate success only after all required children succeed  

## Verification

```bash
go test ./internal/childattempt/
```
