# ROADMAP

<!--
Template for loopcoder work units.

Fields:
- id: Stable short identifier used by depends_on.
- title: Short human-readable work unit title.
- scope: Brief description of what is included in the work unit.
- depends_on: List of work unit ids that must finish first; use [] when none.

The example below is illustrative only, not a real roadmap.
-->

```yaml
- id: docs-example
  title: Add example docs page
  scope: Create a short documentation page for one workflow.
  depends_on: []

- id: checks-example
  title: Add docs link check
  scope: Add a lightweight check that verifies the docs page is linked.
  depends_on:
    - docs-example
```
