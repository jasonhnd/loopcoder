# loopcoder Process

This is the canonical mandatory development workflow for loopcoder. It applies
to every unit of work.

The workflow is strict, ordered, non-skippable, and never bundled.

## 1. Document first

Start with a documentation issue. A worker writes the design or spec document
under `docs/`, then the document is reviewed and merged.

The merged document is the contract. It is a hard prerequisite gate for any
code.

## 2. Code from the doc

Only after the design document is merged, open a separate code issue. The code
issue must reference the merged document, for example:

```text
implement per docs/<doc>.md
```

A worker then writes code strictly following that merged document. The code
change is reviewed and merged separately.

## 3. Verify last

After code exists, verify the implementation against the document. Verification
checks both that the code matches the spec and that it works.

## Hard rules

- Never open a code issue before its design document is merged.
- Never put documentation changes and code changes in the same issue or PR.
- Keep one concern per PR.
- Never hand-write code outside this loop.
