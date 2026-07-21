# v0.9 Global Home Layout (V090-005)

Package: `internal/home` (`V09Layout`)  
Issue: #1097

## Topology

```text
$LOOPCODER_HOME/
  data/machine.db
  projects/<project-id>/
    project.db
    runs/
    logs/
    tmp/
    recovery/
```

## Rules

- All runtime paths resolve under validated absolute `$LOOPCODER_HOME`.
- Directories are owner-only (`0700`); symlinks and group/other bits fail closed.
- Project IDs are single path segments (no `/`, `..`, or absolute forms).
- `OpenMachine` / `OpenProject` go through `authoritystore` after layout ensure.
- Customer repositories must stay free of `.loopcoder`, `machine.db`, `project.db` runtime artifacts (`ScanRepoForRuntimeState`).

## API

- `ResolveV09` / `NewV09` / `EnsureMinimumLayout`
- `EnsureBase` / `EnsureProject`
- `OpenMachine` / `OpenProject`
- `ScanRepoForRuntimeState` / `AssertNotUnderRepo`
