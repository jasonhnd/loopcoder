# Ordered integration receipts and conflict boundary (V090-100)

Package: [`internal/integrationreceipt`](../../internal/integrationreceipt)  
Issue: [#1173](https://github.com/jasonhnd/loopcoder/issues/1173)

## Purpose

Integrate accepted completion candidates into one integration worktree in stable
order with durable receipts. Conflict stops with owner attention — never auto
model-resolve, force, or dual integrators.

## Rules

- Intent freeze: worktree, branch, parent, method, ordered candidate IDs, idempotency key  
- One candidate at a time; receipt before advance  
- Idempotent retry adopts applied results  
- WorkItem closes only after apply + read-back + receipt  

## Verification

```bash
go test ./internal/integrationreceipt/
```
