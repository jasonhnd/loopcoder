# ROADMAP

<!--
Template for loopcoder work units.

Format:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.

The example below is illustrative only, not a real roadmap.
-->

## Example docs page
Create a short documentation page for one workflow.

- doc: Design the example docs page
- code: Add the example docs page

## Example checks
Add a lightweight check that verifies the docs page is linked.

- code: Add docs link check (needs: example-docs-page/code-1)

## [epic] Example migration
Describe one large task here. compile will emit an epic slice DAG.

- doc: Design the migration slice plan
- code: Add the first isolated migration slice
