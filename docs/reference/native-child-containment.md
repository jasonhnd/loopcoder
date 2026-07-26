# Provider-native sub-agent containment (V090-062)

Package: [`internal/nativechild`](../../internal/nativechild)  
Issue: [#1174](https://github.com/jasonhnd/loopcoder/issues/1174)

## Purpose

Owner-approved native sub-agents stay under one LoopCoder Attempt, route pin,
process tree, and resource budget. They are **evidence**, not WorkItems.

## Rules

- Policy `forbidden` → invocation flag exact; any observed child blocks terminal  
- Usage aggregates parent+children against **shared** ceilings (not N×)  
- Status never infers completion from child prose  
- Parent cancel joins all; escape → attention + terminal blocked  

## Verification

```bash
go test ./internal/nativechild/
```
