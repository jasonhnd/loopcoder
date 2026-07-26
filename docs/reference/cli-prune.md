# Prune legacy CLI commands and superseded specifications (V090-078)

Package: [`internal/cliprune`](../../internal/cliprune)  
Issue: [#1192](https://github.com/jasonhnd/loopcoder/issues/1192)

## Purpose

Remove command wiring, help, examples, and specs for deleted systems so users do
not see unsupported choices or invoke compatibility internals by accident.

## Visibility

| Visibility | Help / completions | Invoke |
| --- | --- | --- |
| `supported_v09` | yes | yes |
| `explicit_compat` | yes (tagged) | yes |
| `hidden_removed` | no | fail closed (if deletion evidence OK) |

Commands stay wired until `ReplacementEvidenceOK` and deletion issue evidence.

## Historical specs

`HistoricalSpecs` are non-authoritative and never `compiler_active`.

## Verification

```bash
go test ./internal/cliprune/
```
