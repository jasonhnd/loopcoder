# Redaction and Public-Safety Record

The repository is public. The suspension evidence originated in local
development, provider observation, private consumer-canary, and runtime
directories. This document records how the retained evidence was made safe
enough for an unpublished draft in the same public repository.

## 1. Boundary

Two asset classes required different handling:

- **text evidence** could be structurally transformed and revalidated;
- **exact RC archives** could not be modified without changing their historical
  digests.

The text evidence was sanitized. The RC archives were scanned and retained
byte-for-byte.

## 2. Text Evidence Inputs

The sanitizer processed 61 files:

- JSON;
- JSON Lines;
- stderr and stdout logs;
- SHA-256 records;
- Git mail patches;
- binary Git diffs represented as text patches;
- ref, branch, worktree, object, and size inventories;
- small lifecycle and recovery records.

The sanitizer refused:

- symbolic links;
- files containing NUL bytes;
- unsupported filesystem object types.

No binary or database was routed through the text sanitizer.

## 3. Structured Transformations

JSON was parsed before transformation. Sensitive keys were handled by class.

### 3.1 Secret values

These key classes were replaced with `[SECRET_REMOVED]`:

- access tokens;
- refresh tokens;
- ID tokens;
- session tokens;
- API keys;
- client secrets;
- passwords;
- authorization values;
- cookies;
- raw credential objects.

Four JSON values were removed by sensitive-key or email policy.

### 3.2 Account and machine identifiers

These identifiers were replaced with stable 12-hex SHA-256 pseudonyms:

- account profile IDs;
- machine IDs;
- device IDs;
- user IDs.

The stable pseudonym preserves equality relationships inside the archive
without exposing the original value. It does not allow recovery of the source
identifier without the original input.

The run pseudonymized 452 identifier values.

### 3.3 Email addresses

Email-valued fields and email-like strings were replaced with
`[EMAIL_REDACTED]`.

### 3.4 Local paths

Local home prefixes were replaced with `${HOME}`. Local temporary prefixes were
replaced with `${TMPDIR}`.

The replacement retains enough path shape to explain inventories and restore
relationships without publishing the workstation username or random temporary
root.

### 3.5 Private repositories

Names matching the private disposable v0.9 consumer-repository namespace were
replaced with `jasonhnd/[PRIVATE_REPO_REDACTED]`.

The public repository name `jasonhnd/loopcoder` was retained.

## 4. Unstructured Secret-Shape Scan

Text values were scanned for common high-risk forms:

- GitHub classic and fine-grained tokens;
- OpenAI keys;
- Anthropic keys;
- xAI keys;
- Slack tokens;
- bearer authorization values;
- JWTs;
- email addresses;
- credential-like query parameters;
- workstation home paths;
- private disposable repository names.

The sanitizer made 260 text replacements.

This is a bounded pattern scan, not a proof that every possible secret format
in every provider ecosystem is enumerable. The stronger safety property is
that raw provider homes, session stores, databases, and credential files were
excluded entirely.

## 5. Post-Transformation Validation

After transformation:

- every `.json` file passed `jq empty`;
- JSON Lines were parsed line by line;
- `parse_fallbacks` was zero;
- the output was rescanned for local paths, private repository names, emails,
  and recognized token shapes;
- no match remained;
- the evidence archive passed `zstd -t`;
- the archive file list was reviewed;
- checksums were calculated after the final transformation.

The machine-readable result is
[`redaction-report.json`](redaction-report.json).

## 6. RC Archive Scan

RC60 and RC61 were retained byte-for-byte because their SHA-256 digests are
part of the historical decision evidence.

For each archive:

1. list archive members;
2. extract into a disposable directory;
3. confirm only `loopcoder`, `LICENSE`, and `README.md` are present;
4. inspect file types;
5. inspect `go version -m`;
6. confirm `-trimpath=true`;
7. scan text files;
8. run `strings -a` over the Mach-O binary;
9. scan for the same local path, private repository, and credential shapes;
10. inspect the ad hoc code-signature metadata;
11. recalculate the archive SHA-256.

No actual workstation username, private consumer-repository name, email, or
recognized credential shape was found.

## 7. Synthetic Privacy Marker

Both binaries contain:

```text
/Users/syn-private/v090067/SECRET_PATH_DDDD
```

This is not a host path. It is the deliberate public test marker defined in:

```text
internal/privacy/markers.go
```

The marker exists so privacy tests can prove that exported evidence does not
leak a forbidden path. It was retained because changing the compiled string
would change the exact archive digest.

The binaries also contain a concatenated `/Users/l...` string fragment caused
by Go string-table packing. It is not a complete local home path and does not
contain the workstation username.

## 8. Excluded High-Risk Inputs

The following were not sanitized and uploaded; they were excluded:

- `.claude.json` and backups;
- Grok session files;
- Gemini project files;
- provider-home directories;
- SQLite runtime databases;
- private repository `.git` directories;
- signed URLs;
- raw provider transcripts not required by the decision record;
- caches and lock files.

Exclusion is safer than attempting to enumerate and transform every possible
secret field in mutable provider state.

## 9. Integrity After Redaction

Redacted evidence is not byte-equivalent to its raw local source. Its purpose
is to preserve:

- event relationships;
- decision fields;
- route and capacity structure;
- candidate identities;
- artifact digests;
- qualifier verdicts;
- patch semantics;
- cleanup inventory.

The redacted archive has its own digest:

```text
ea0bd8ca4654c1e5f04b6d2067a61bc6d62dc8c2d825d7d09d47628082c1c52d
```

That digest proves the uploaded public-safe evidence asset, not the deleted raw
input files.

## 10. Known Limitations

- Pseudonyms prove equality only within this transformation policy.
- Historical quota and provider observations become stale and must not be used
  as current routing authority.
- Logs may omit excluded context.
- The pattern scan cannot define every future credential format.
- The draft is unpublished, but it belongs to a public repository and must
  still be treated as public-safe.
- RC binaries are historical executable code. Their inclusion preserves
  identity; it does not certify safety, support, or acceptance.

## 11. Future Handling

Anyone adding evidence to this snapshot later must:

1. treat the repository as public;
2. exclude provider homes and mutable databases;
3. use structured parsing for structured files;
4. publish the transformation policy;
5. revalidate output syntax;
6. scan final assets;
7. calculate new digests;
8. update the manifest and checksum record;
9. preserve `NO_GO`;
10. keep the draft unpublished unless the owner explicitly chooses another
    archival mechanism.
