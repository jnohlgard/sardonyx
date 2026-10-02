# Sardonyx — Implementation Plan

**Status:** ✅ T1–T11 complete (config, models, transform, local directory
source, git repository source, Onyx client, CLI wiring, summary &
polish, test hardening, build & README, and the `sard check` pre-flight
command, §13).

**Language:** Go (switched from Python, 2026-10 — motivation: a single static
binary that needs no interpreter or virtualenv on the target machine). The
design itself is language-neutral; only the tooling, layout, and library
choices in this document changed.

---

## 1. Goal

A small Go CLI tool that:

1. Takes **one positional input**: a git repository URL **or** a local directory path.
2. Discovers all **Markdown files** (`*.md`, `*.mdx`, `*.markdown`).
3. Transforms each file into a valid Onyx **Ingestion API** payload.
4. POSTs the payloads to the Onyx Ingestion API so the documents appear in the Onyx
   workspace (under a designated Connector / CC-pair).

### Requirements

| ID   | Requirement                                                                                                                                                          |
| ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R1   | Accept a git repo URL (`https://`, `git@` ssh form, or `owner/repo` shorthand) or a local filesystem path as the single positional input.                                                            |
| R2   | Discover Markdown files: `.md`, `.mdx`, `.markdown`.                                                                                                                 |
| R3   | Produce valid Onyx `IngestionDocument` payloads (see §6).                                                                                                            |
| R4   | Send payloads to the Ingestion API with Bearer auth, timeouts, and retries (§7).                                                                                     |
| R5   | **Idempotent re-runs**: deterministic, stable document IDs so re-ingesting the same repo/directory **updates** existing documents instead of duplicating.          |
| R6   | Per-file error isolation: one bad file does not abort the run; print a final summary (new / updated / failed); meaningful exit codes (§8).                         |
| R7   | `--dry-run`: print the payloads that would be sent, send nothing.                                                                                                    |
| R8   | Configuration via CLI flags → environment variables → `.env` file, in that priority order (§5.1).                                                                   |
| R9   | Default exclusion of noise dirs (`.git`, `node_modules`, `dist`, `build`, `vendor`, `.venv`, …) plus `--include` / `--exclude` glob overrides and `--max-depth`.  |
| R10  | Test suite covering transform logic, ID stability, and HTTP behavior against a mocked Onyx server.                                                                  |

### Non-goals (v1)

- **Deleting/pruning** documents from Onyx — the Ingestion API has no delete endpoint, so
  deleted source files leave stale documents behind. Mitigation is metadata/naming; a future
  version may call a separate Onyx deletion API if one exists.
- Non-Markdown file types (PDF, DOCX, …).
- Scheduling / continuous sync (a cron job or CI step can invoke the CLI).
- Image sections (`ImageSection` requires a separate file-store upload round-trip).
- Creating the Onyx Connector / CC-pair programmatically (user creates it in the Admin Panel
  first — prerequisite, see §11).

---

## 2. Background: the Onyx Ingestion API

Full reference notes in [`docs/onyx-ingestion-api.md`](onyx-ingestion-api.md).
Source: <https://docs.onyx.app/developers/guides/index_files_ingestion_api>

- **Endpoint:** `POST {API_BASE_URL}/onyx-api/ingestion` — cloud base is
  `https://cloud.onyx.app/api`; self-hosted deployments use your own domain.
- **Auth:** `Authorization: Bearer {API_KEY}`. Requires the `manage:connectors` permission
  (or `admin`). Group managers may ingest into CC-pairs of groups they manage (not the
  default public pair).
- **Request body:** `{"document": {…IngestionDocument fields…}, "cc_pair_id": <int>}`.
  `cc_pair_id` is required for the document to show up on the Connectors page; it is visible
  in the Admin Panel URL (`/admin/connector/<cc_pair_id>`).
- **Key document fields we use:** `id` (stable → upsert), `semantic_identifier` (UI name),
  `title` (search name), `sections` (`text` + `link`), `source` (enum: `file`, `github`,
  `gitlab`, `web`, `ingestion_api`, …), `metadata` (str or list[str] values → tags),
  `doc_updated_at`, `from_ingestion_api`.
- **Response:** `200` means *accepted*; body contains `already_existed` (true ⇒ updated,
  false ⇒ created). Actual indexing (OpenSearch) happens asynchronously.

---

## 3. Architecture

```
                    ┌──────────────────┐    ┌───────────────────┐    ┌────────────────┐
 input (url | path)→│ 1. Source layer  │ →  │ 2. Transform layer│ →  │ 3. Onyx client │ → Onyx Ingestion API
                    │  git repo /      │    │  Markdown →       │    │  HTTP + retry  │
                    │  local directory │    │  IngestionDoc     │    │  + summary     │
                    └────────┬─────────┘    └────────┬──────────┘    └────────────────┘
                             │
                    ┌────────┴─────────┐
                    │ 0. Config        │  CLI flags > env vars > .env
                    └──────────────────┘
```

- **Source layer** (one Go file per input kind, package `source`) answers: *"which files
  exist, what is their content, and what are their provenance facts?"* It emits a slice of
  `models.IngestedFile` records.
- **Transform layer** maps each `IngestedFile` → an `models.OnyxPayload` (pure function,
  no I/O).
- **Onyx client** handles all HTTP concerns and returns per-document results
  (`created | updated | failed` + reason).
- **CLI** wires the pipeline together and prints the summary.

Processing is **sequential** in v1 (simple, kind to rate limits); a `--concurrency` option is
a documented future extension.

---

## 4. CLI specification

```
sard ingest <source> [options]

<source>
  Git repo URL  e.g. https://github.com/owner/repo  |  git@github.com:owner/repo.git  |  owner/repo
  or a local directory path, e.g. ./docs

Options
  --api-url        Onyx API base URL        (default https://cloud.onyx.app/api; env ONYX_API_URL)
  --api-key        Onyx API key             (env ONYX_API_KEY)
  --cc-pair-id     Onyx connector-credential-pair id, int  (env ONYX_CC_PAIR_ID)
  --branch         Branch to clone; git URLs only (default: repo's default branch)
  --source         Override the document "source" enum: file | github | gitlab | web | ingestion_api
  --id-base        Arbitrary ID base for every document of this run (env SARD_ID_BASE).
                   Default: normalized origin URL (git) or cleaned absolute root path (local).
                   Keeps IDs stable across forks / URL changes (git) or moved dirs, mount
                   points, and symlinks (local); affects the document ID only (§6)
  --include GLOB   Include filter, repeatable (e.g. "docs/**", "*.mdx")
  --exclude GLOB   Exclude filter, repeatable (on top of the default noise-dir exclusions)
  --max-depth N    Max directory depth below the source root
  --max-file-size  Skip files larger than N KiB (default 1024)
  --token          Git auth token for private repos (env GIT_TOKEN); injected as x-access-token in the HTTPS URL
  --dry-run        Print payloads to stdout; send nothing (needs no Onyx
                   credentials — see the T10 decision below)
  --limit N        Ingest at most N files (smoke-testing)
  --log-level      debug | info | warning | error (default info)
```

Implementation notes:

- Single subcommand. `spf13/cobra` provides the root `sard` command and
  the `ingest` subcommand; the flag definitions on `ingest` are the single
  source of truth for the flag list in `--help` — cobra renders the usage
  line and the option list from the same definitions that bind the parsed
  values, so the two cannot drift apart. The repeatable `--include` /
  `--exclude` flags are pflag's `StringArray` (no comma splitting, one
  value per occurrence). The `ingest` help output is a summary paragraph
  (the command's Long text), the usage line, an Arguments section
  describing `<source>`, a static Environment section summarizing the
  env vars behind the flags (the names are hand-written and kept in
  sync with `internal/config` by `TestIngestUsageTemplateEnvNames`),
  and the flag list — all hand-written additions to the usage
  template, below the usage line.
- Logging via `log/slog` (level from `--log-level`); log output goes to stderr.
- Root-level `--version` (and `-v`), a cobra built-in: it prints the
  version string stamped at build time (`main.version`, README
  "Building"; default `dev` for plain builds) to stdout and exits 0.
  `cmd/sard` forwards the stamp to `cli.Version`; the flag exists on
  the root command only, so `sard ingest --version` is an unknown flag
  (exit 2).

Decisions made while implementing T7 (recorded in the `internal/cli` package doc):

- `cli.Run(args) (exitCode int)` keeps the stub's signature. The pipeline logs
  everything through a `*slog.Logger` built by `Run` from `--log-level` (stderr);
  `cmd/sard/main.go` prints nothing and only maps the returned code to `os.Exit`.
- Bad flag values — unknown `--source`, negative `--limit` / `--max-depth` /
  `--max-file-size` / `--cc-pair-id`, invalid `--log-level`, unknown flags, a
  wrong number of arguments, and unrecognized inputs (bad path, invalid git URL
  form) — are all configuration errors, exit 2, checked in one pre-flight step.
- `--dry-run` exits 0 whenever discovery succeeds (it sends nothing, so no file
  can fail; exit 1 applies to real ingestion runs only). A discovery error in
  dry-run still exits 2.
- `--limit N` caps the number of files ingested — and the number of payloads
  printed in a dry-run — and the summary's total (the T8 summary describes
  the capped set; the skipped count comes from discovery, pre-limit).
- `--branch` and `--token` are git-only flags: for a local directory input they
  are ignored with a warning (their values are never logged).
- An Onyx 401/403 (`onyx.ErrAuth`) aborts the run with exit **2**: a rejected
  key is a credentials problem, and every remaining file would fail identically
  (see §7 fail-fast, §8).
- pflag parses flags and the positional `<source>` in any order, so both
  `sard ingest ./docs --dry-run` and `sard ingest --dry-run ./docs` work.
  (T7 originally compensated for stdlib `flag` stopping at the first
  positional argument with a re-sorting pre-parser; the cobra migration
  removed it.)

Decisions made while implementing T8 (summary & polish):

- **Skipped count.** `source.LocalResult` and `source.GitResult` now carry a
  `Skipped` count: files dropped by a per-file check (oversize, binary,
  empty, unreadable; the git source also counts files without commit
  metadata). Each drop already warns per file; the count feeds the summary
  and the 0-files warning. Files outside the depth/include/exclude scope are
  not counted — that is deliberate scoping, not a skip.
- **Elapsed scope.** The summary's elapsed time spans the whole run — the
  timer starts in `Run` before discovery (for a git source the clone
  dominates), not just the ingest loop.
- **Summary shape.** The end of every run prints a header line plus one
  follow-up line per failed file, each a separate slog record — a single
  multi-line message would be `\n`-escaped by the TextHandler, defeating
  the purpose. The header is `run complete: <N> files in <elapsed> —
  created <c>, updated <u>, skipped <s>, failed <f>`; the failure lines are
  `  failed: <file> — <reason>`, in ingestion (RelPath) order. A dry run
  reports `dry run complete: … printed <p> payloads, skipped <s>` instead;
  an interrupted run prints the ingest shape with `run interrupted` at warn
  level. The live per-file failure warn is kept (it is the progress feed);
  the summary re-lists the failures in one place, as §8 requires.
- **Testing the summary.** A real Onyx instance is not available; the
  `httptest` mock is the reference for the summary (recorded in the T8
  acceptance line). The harness captures stderr as well as stdout, so the
  summary is asserted like any other output.

Decisions made while implementing T9 (test hardening):

- **Coverage gate, per function (Q1).** The gate "≥ 90% on URL
  normalization" is read per function, not per package: the source
  package as a whole stays under 90% (its clone/collect I/O is
  exercised by integration tests, which coverage does not count), so
  the gate is ≥ 90% for each of the URL-normalization functions in
  `git.go` (NormalizeURL, remoteOrigin, fileOrigin, userinfo,
  sourceForHost, repoLabel, stripGitSuffix, CloneURL, sectionLink,
  redact), plus ≥ 90% for the `config` and `transform` packages.
  After T9: all ten functions at 100% (both known gaps closed — the
  `url.Parse` error branch, `stripGitSuffix`'s slash-less path),
  `config` at 93.5%, `transform` at 100%. T10 inherits this reading
  of the acceptance line and leaves it unchanged (the dry-run
  pre-flight addition lands `config` at 93.6%).
- **"Very long single file" (Q2).** Interpreted as (a) a large
  (512 KiB) single-line, in-limit file ingests cleanly with a stable
  ID and the basename title, and (b) the oversize boundary: a file
  exactly at `--max-file-size` is kept, one byte over is skipped. No
  multi-MB content is manufactured; the suite stays fast.
- **CRLF on the git path (Q3).** Covered by a dedicated fixture
  (TestGitCRLF): the fixture repo commits with
  `core.autocrlf=false`, and hermetic `GIT_CONFIG_GLOBAL` /
  `GIT_CONFIG_SYSTEM` pin the clone's checkout, so line endings cannot
  be rewritten by the test machine's configuration.
- **Config per-function gaps (Q4).** Not closed: the gate is per this
  plan's wording (the `config` package at ≥ 90%), which already passes
  at 93.5%; `Error` (66.7%) and `loadDotEnv` (83.3%) stay as-is rather
  than manufactured.
- **Frontmatter and titles.** The §6 naive "first `# ` line" title is
  kept. T9 asserts that fence lines and key/value bodies never act as
  the heading, that the first heading after the fence is the title,
  and that content is ingested verbatim (no stripping — §6 keeps
  frontmatter as-is in v1). A YAML *comment* line inside a fence would
  still be picked up by the naive scan; a fence-aware title is a v2
  item, not a T9 fix.

Decisions made while implementing T10 (build & README):

- **A dry run needs no Onyx credentials.** The pre-flight used to
  require `ONYX_API_KEY` and `ONYX_CC_PAIR_ID` for every invocation,
  which blocked T10's acceptance line (a fresh checkout must run
  `sard ingest <dir> --dry-run` with zero configuration). A dry run
  sends nothing, so the requirement no longer applies to it:
  `config.Flags` carries a `DryRun` marker and `Resolve` skips the two
  required-value checks when it is set. A cc-pair id that is *present*
  but not an integer is still an error in both modes (the validation is
  kept; the requirement is not). A dry run without one prints
  `"cc_pair_id":0`; providing a value puts it in the preview. `--token`
  / `GIT_TOKEN` is still honoured for dry runs of git sources — the
  clone authenticates even though no ingest does. Tests: the config
  pre-flight in both modes and a zero-credentials cli dry run.
- **No release pipeline.** There is no artifact to link, so the README
  documents building from source (the static-build recipe, and
  `go install sardonyx/cmd/sard` from a checkout) and says so.

Decisions (cobra migration, post-T10):

- **cobra instead of stdlib `flag`.** The stdlib made T7's `ingest`-level
  `--help` auto-generated, but the top-level usage was hand-written text
  and the positional-first form needed the `reorderFlags` pre-parser — a
  hand-rolled argument parser, exactly where edge-case bugs live.
  `spf13/cobra` (the de facto standard for Go CLIs: `kubectl`, `gh`,
  `hugo`) removes both: the usage line and the option list render from
  the flag definitions themselves. Both commands run with
  `SilenceErrors`/`SilenceUsage`, so `Run` keeps sole ownership of all
  diagnostics/usage output and of the exit codes; `cli.Run`'s signature
  is unchanged and `cmd/sard` stays a one-line wrapper.
- **Help conventions follow cobra.** `--help` (and `-h`, new) prints
  auto-generated usage to **stdout** and exits 0 (previously stderr;
  bare `sard --help` was exit 2). `sard` without a subcommand and
  `sard <bogus>` print the auto-generated top-level usage to stderr and
  exit 2 — a trimmed usage template without the misleading
  "sard [flags]" line; the hand-written usage block is gone, and the
  `<source>` description lives in an Arguments section of the ingest
  usage template, below the usage line (help's Long is a summary).
  `sard help` and `sard completion <shell>` are cobra built-ins. Every
  other exit-code behavior in §8 is unchanged, including the
  configuration-error matrix and the SIGINT paths (verified by the
  existing `cli_test.go` suite, unmodified).
- **`--version` activates the documented build stamp (post-T10).** T10's
  build recipe (`-X main.version=…`) and the `var version = "dev"` in
  `cmd/sard` predated the flag — the stamp was dead. Setting the root
  command's `Version` (a new `cli.Version` variable, default `dev`,
  assigned in `main` from the stamp) switches on cobra's built-in
  `--version`/`-v`: `sard --version` prints `sard version <version>`
  to stdout and exits 0, like git's `git version`. The flag is
  root-only; covered by `TestRunVersion`.

Examples (documented in the README's Getting started):

```bash
sard ingest https://github.com/onyx-dot-app/onyx --cc-pair-id 42
sard ingest ./my-docs --api-url http://onyx.local:8080/api --api-key $ONYX_API_KEY
sard ingest https://github.com/org/repo --include "docs/**" --dry-run
```

There is also a `sard check` subcommand (T11, §13): it validates the
configuration and verifies Onyx connectivity and credentials without
creating any document — the pre-flight to run before a first real ingest.

---

## 5. Source layer

### 5.1 Configuration resolution

Priority: **CLI flag > environment variable > `.env` file (current working directory) > default**.
Env vars: `ONYX_API_URL`, `ONYX_API_KEY`, `ONYX_CC_PAIR_ID`, `GIT_TOKEN`, `SARD_ID_BASE` (optional, §6).
`.env` loading via `github.com/joho/godotenv`; a missing `.env` is not an error.
Resolved into a `config.Settings` struct (T1); required values missing after all sources
are a configuration error (exit code 2), detected pre-flight.

### 5.2 Git repository source

- **URL normalization** (pure function, unit-tested):
  - `https://host/owner/repo[.git]` → unchanged (token-injection point preserved).
  - `git@host:owner/repo[.git]` → `https://host/owner/repo` (we never clone over SSH).
  - `owner/repo` shorthand → `https://github.com/owner/repo`.
  - Host detection → default `--source` value: `github.com` → `github`, `gitlab.*` → `gitlab`,
    anything else → `file`.
- **Cloning:** `os/exec` runs `git clone --depth 1 [--branch <b>] <url> <tmpdir>` into a
  fresh temp dir from `os.MkdirTemp` (shallow = fast, small); the temp dir is removed via
  `defer os.RemoveAll` on exit (even on error).
- **Private repos:** when `--token`/`GIT_TOKEN` is set, clone URL becomes
  `https://x-access-token:<token>@host/owner/repo` (token never logged or echoed).
- **File discovery:** use `git ls-files '*.md' '*.mdx' '*.markdown'` against the clone.
  This lists **tracked** files only, so the repo's own `.gitignore` is respected for free —
  no manual noise-dir walking needed for this path.
- **Filtering:** the same `--include`/`--exclude`/`--max-depth`/`--max-file-size` options as
  the local source apply; `GitOptions` mirrors `LocalOptions` (with `--branch`/`--token` as the
  git-specific additions) so the pipeline (T7) treats both sources uniformly.
- **Provenance per file:** `git log -1 --format=%H%x00%ct -- <path>` → commit SHA (→
  `metadata.commit`) and commit unix timestamp (→ `doc_updated_at`). One `git log` call per
  file is acceptable for v1 (batching is a future optimization). With a `--depth 1` clone the
  history holds only the HEAD commit, so every file is attributed to HEAD — a safe upper bound
  for Onyx freshness; the per-file `git log` form becomes exact if the depth is ever raised.
- **Section link:** for `https://github.com/…` (public) URLs, blob URL
  `https://github.com/owner/repo/blob/<branch>/<path>`; otherwise `null`.
- **Default ID base:** the normalized origin URL with a lowercased host and
  no trailing `.git` (e.g. `https://github.com/owner/repo`). Never includes
  credentials, never logged. A `--id-base` / `SARD_ID_BASE` override
  replaces it for the whole run, used verbatim (§6).

### 5.3 Local directory source

- Validate the path exists and is a directory (else config error, exit code 2).
- If the directory (or an ancestor) is a git repo, discovery prefers `git ls-files` (tracked
  files only; the repo's own `.gitignore` is respected for free) and falls back to a warned
  `filepath.WalkDir` when git is unavailable or fails. The input type decides the kind:
  a directory input always yields `local` records — `doc_updated_at` is the file's mtime and
  `CommitSHA`/`BlobURL` stay empty, so a repo's commit metadata is never ingested on this
  path (a git repo URL input is what gets commit provenance, §5.2).
- **Default exclusions** (walk mode only): `.git`, `node_modules`, `dist`, `build`,
  `vendor`, `.venv`, `__pycache__`, `.idea`, `.github` (keep README-adjacent dirs? —
  decision: include `.github` workflows dir? No: exclude; `--include` can override).
- `--max-depth` and `--include`/`--exclude` globs via `github.com/bmatcuk/doublestar/v4`,
  matched against the file path **relative to the source root** with forward slashes.
  Semantics: `*` does not cross path separators, `**` does (`docs/**` matches everything
  under `docs/`); matching is case-sensitive on Unix.
- Provenance: `doc_updated_at` = file mtime (UTC RFC-3339); `metadata.path` = relative path.
- Content read as UTF-8: invalid byte sequences are replaced with U+FFFD (small helper over
  `unicode/utf8`). Files containing NUL bytes are treated as binary and skipped with a
  warning.
- **Default ID base:** the source root's cleaned absolute path as given (no
  symlink resolution). A `--id-base` / `SARD_ID_BASE` override replaces it
  for the whole run, used verbatim (§6).

### 5.4 Common output record

```go
// models.IngestedFile — one discovered file, ready for transform.
type IngestedFile struct {
	Kind         string    // "git" | "local"
	RootLabel    string    // "owner/repo" or basename(abs(dir))
	RelPath      string    // relative to the source root, forward slashes
	Content      string    // Markdown text
	DocUpdatedAt time.Time // commit time (git) or mtime (local), UTC
	CommitSHA    string    // git source only
	BlobURL      string    // github.com source only
}
```

---

## 6. Transform: Markdown → Onyx document

Pure function `ToOnyxPayload(f models.IngestedFile, source string, ccPairID int, idBase string)
models.OnyxPayload`. The payload is a typed struct with JSON tags matching the Onyx schema
(marshaled with `encoding/json`); the top-level request envelope is
`{"document": {…}, "cc_pair_id": n}`. The pipeline (T7) computes the run's `idBase` once:
the `--id-base` / `SARD_ID_BASE` override when set, else the source's default
(§5.2 / §5.3).

| Field                | Value (git)                                              | Value (local)                                  |
| -------------------- | -------------------------------------------------------- | ---------------------------------------------- |
| `id`                 | `sha256("git\0" + idBase + "\0" + relpath)`              | `sha256("local\0" + idBase + "\0" + relpath)`  |
| `semantic_identifier`| `owner/repo/path/to/file.md`                             | `<dir-name>/path/to/file.md`                   |
| `title`              | first `# …` heading in the file, else the filename       | same                                           |
| `sections`           | `[{"text": content, "link": blob_url or None}]`          | `[{"text": content}]`                          |
| `source`             | `github` / `gitlab` / `file` (per §5.2; `--source` override) | `file` (or override)                   |
| `metadata`           | `{repo: <owner/repo>, path: <relpath>, commit: <sha>, ingested_by: "sardonyx"}` | `{path: <relpath>, ingested_by: "sardonyx"}` |
| `doc_updated_at`     | last-commit timestamp, RFC-3339 UTC                      | mtime, RFC-3339 UTC                            |
| `from_ingestion_api` | `true`                                                   | `true`                                         |

- **ID design note:** branch is deliberately *not* part of the git ID — re-ingesting a
  different branch updates the same documents (last write wins). Two different branches
  ingested into the same CC-pair will overwrite each other; this is acceptable and documented.
- **ID base & migrations.** The ID is derived from an *ID base* that is a property of the
  run, not of the file: the git source's normalized origin URL (§5.2) or the local root's
  cleaned absolute path (§5.3), with the file's `relpath` as the per-file part.
  `--id-base` / `SARD_ID_BASE` replaces the base for the whole run, used verbatim — any
  string, no normalization. The override affects the ID only: `semantic_identifier`,
  `metadata`, and section links still reflect the actual origin/path. Uses:
  - **Fork / URL change:** passing the *old* origin as the base keeps documents matching
    the fork (Onyx sees updates, not duplicates).
  - **Moved directory / mount point / symlink:** passing the *old* absolute path — or any
    canonical string — keeps IDs stable. For long-term stability, pick one canonical base
    per project (e.g. a project name) and always pass it, so moves, forks, and renames
    never change an ID.
  - Changing the effective base (or letting the default follow a changed origin/path)
    mints a fresh ID set; the previous documents stay in Onyx as stale (§11 #1).
  - The kind prefix (`git` / `local`) keeps the two ID spaces disjoint.
- **Intentionally omitted:** `chunk_count` (let Onyx compute), `primary_owners` /
  `secondary_owners`, `additional_info`, image sections.
- **Content:** raw Markdown text in a single section. v2 candidate: split into sections per
  top-level heading (better citations/links) — noted as a future task, not planned now.
- Frontmatter (YAML at top of `.mdx`/Jekyll files) is kept as-is in v1; stripping is a
  future option.

---

## 7. Onyx client

`onyx.NewClient(apiURL, apiKey string, ccPairID int) *Client`:

- `client.Ingest(ctx context.Context, payload models.OnyxPayload) (IngestResult, error)` —
  one POST per document to `{api_url}/onyx-api/ingestion`.
  - Headers: `Authorization: Bearer <key>`, `Content-Type: application/json`;
    `http.Client{Timeout: 30 * time.Second}`.
  - **Retries:** up to 3 attempts with exponential backoff (1 s, 4 s, 16 s) on
    `429`, `5xx`, and connection/timeout errors; `ctx.Err()` is checked between attempts so
    Ctrl-C aborts promptly. Never retry other 4xx.
  - **Fail-fast:** `401`/`403` → return immediately with an actionable message
    ("check ONYX_API_KEY and that it has `manage:connectors` or `admin`").
  - **Result mapping:** `200` → `created` if `already_existed` is falsy, `updated` otherwise;
    non-200 after retries → `failed` with status code + body excerpt.
- Never logs the API key.

Implementation decisions (T6, recorded after the fact):

- **`cc_pair_id` reconciliation:** `Ingest` sends the payload's own
  `CCPairID` — the transform stamps it from the same `Settings.CCPairID`,
  so the payload is the single source of truth on the wire; the
  constructor's `ccPairID` parameter is kept only for signature
  compatibility with this section.
- **Backoff injectability:** the wait schedule lives in an unexported
  client field (default 1 s / 4 s / 16 s per the spec); tests replace it
  with millisecond values, so the retry tests stay fast while production
  keeps the plan's schedule. With the 3-attempt cap only the first two
  waits fire; the 16 s entry is kept so a raised cap has its slot.
- **`api_url` handling:** the config layer guarantees the base URL
  (default `https://cloud.onyx.app/api`); the client tolerates a trailing
  `/` by stripping it before joining the `/onyx-api/ingestion` path.
- **Error contract:** `401`/`403` return a non-nil error wrapping the
  exported `ErrAuth` sentinel (fail-fast: the caller stops the run, since
  every later document would fail the same way). Other failures (retries
  exhausted, non-retryable 4xx) return a `failed` result with a nil error,
  so one bad document never aborts a run; context cancellation returns a
  `failed` result with the context's error (exit 130, §8).

---

## 8. Error handling & exit codes

| Code | Meaning                                                             |
| ---- | ------------------------------------------------------------------- |
| 0    | Run completed; all discovered files ingested successfully (or 0 files found, with a warning) |
| 1    | Run completed but ≥1 file failed to ingest                          |
| 2    | Configuration error (missing api key / cc_pair_id — real runs only, bad path, bad flags) |
| 130  | Interrupted (Ctrl-C)                                                |

- A `--dry-run` is the one case that needs no Onyx credentials: it
  sends nothing, so a missing API key / cc-pair id is not a
  configuration error for it (decision, §4 T10).
- Ctrl-C: `main` runs the pipeline under `signal.NotifyContext(context.Background(),
  os.Interrupt)`; on interrupt, exit with 130.
- Per-file failures are logged, recorded in the summary, and do not abort the run.
- Onyx authentication failures (401/403, `onyx.ErrAuth`) abort the run with 2:
  the client fails fast on the first such file because every remaining file
  would fail identically (§7) — a credentials problem, not a per-file failure.
- Summary printed at the end of every run (info level; warn when the run was
  interrupted): a header line with the total file count, the whole-run elapsed
  time (discovery to final action — the git clone dominates), and the
  `created` / `updated` / `skipped` (per-file checks) / `failed` counts — a
  dry run reports the payloads printed and `skipped` instead — followed by
  one `failed: <file> — <reason>` line per failed file in ingestion order.
  During a real run, every file also gets a progress line
  `[i/N] <file> → created|updated|failed`. All of it goes to stderr; stdout
  stays reserved for `--dry-run` JSON and the `--version` output.

---

## 9. Repository layout

```
sardonyx/
├── go.mod                       # module sardonyx (deps: godotenv, doublestar/v4, cobra)
├── README.md                    # project overview, getting started, build, usage
├── .env.example                 # ONYX_API_KEY / ONYX_API_URL / ONYX_CC_PAIR_ID / GIT_TOKEN
├── .gitignore
├── docs/
│   ├── PLAN.md                  # this file
│   └── onyx-ingestion-api.md    # condensed reference for the Ingestion API
├── cmd/
│   └── sard/
│       └── main.go              # thin entry point: version stamp, run, exit code
└── internal/
    ├── cli/
    │   ├── cli.go               # pipeline orchestration, summary, exit codes
    │   └── cli_test.go          # end-to-end smoke test vs httptest
    ├── config/
    │   ├── config.go            # flags > env > .env resolution
    │   └── config_test.go
    ├── models/
    │   └── models.go            # IngestedFile, IngestResult, OnyxPayload
    ├── source/
    │   ├── git.go               # normalize URL, clone, git ls-files, commit metadata
    │   ├── git_test.go
    │   ├── local.go             # walk / git ls-files, filters, mtime
    │   ├── local_test.go
    │   └── testdata/            # fixture tree (local source); git fixture repo built in a temp dir
    ├── transform/
    │   ├── markdown.go          # IngestedFile → OnyxPayload
    │   └── markdown_test.go
    └── onyx/
        ├── client.go            # HTTP, retries, result mapping
        └── client_test.go       # against net/http/httptest
```

Go conventions: tests live next to the code in the same package (`*_test.go`); there is no
separate top-level `tests/` directory.

---

## 10. Implementation tasks (in order)

Each task should land in its own commit with passing tests where applicable.
Run with `go test ./...`.

- **T1 — Config & environment** (`internal/config/`)
  - Implement flag > env > `.env` resolution into a `Settings` struct (`godotenv`);
    pre-flight validation of required values (missing → exit 2).
  - Accept: ✅ unit tests for precedence; missing key detected pre-flight (exit 2).
- **T2 — Models** (`internal/models/`)
  - `IngestedFile`, `IngestResult`, `OnyxPayload` structs with JSON tags.
  - Accept: ✅ compiles, used by later packages.
- **T3 — Transform** (`internal/transform/`)
  - `ToOnyxPayload(f, source, ccPairID, idBase)` exactly per §6 (ID = sha256 over
    kind + ID base + relpath, title-from-heading, metadata, timestamps).
  - Accept: ✅ unit tests: deterministic ID across two calls; branch not in ID; same
    idBase + relpath → same ID regardless of actual origin/root (the migration case);
    different bases → different IDs; local vs git shapes (kind prefix, link presence);
    first-heading title; no-heading fallback; RFC-3339 UTC formatting.
- **T4 — Local directory source** (`internal/source/local.go`)
  - `filepath.WalkDir` + default exclusions + `doublestar` include/exclude + max-depth +
    max-file-size + mtime.
  - Accept: ✅ tests with a fixture tree under `testdata/` (incl. nested noise dirs);
    empty-file skip.
- **T5 — Git repo source** (`internal/source/git.go`)
  - URL normalization, shallow clone via `os/exec` into an `os.MkdirTemp` dir (deferred
    cleanup), `git ls-files`, per-file `git log` metadata, token injection, blob URLs.
  - Accept: ✅ unit tests for URL normalization (pure fn, table-driven); integration test
    that clones a fixture repo built in a temp dir (a repo under `testdata/` would nest a repo
    in a repo; local-path clone via `file://` so `--depth` is honored; skipped when `git` is
    not on PATH) and yields expected files + metadata; token-injection test against a
    401-challenging `httptest` server verifying the token reaches the wire and never the logs.
- **T6 — Onyx client** (`internal/onyx/`)
  - POST + auth + timeout + retries + fail-fast + result mapping (per §7).
  - Accept: ✅ tests against a `net/http/httptest` server (stdlib only):
    success (new/updated from `already_existed`), 429 then 200, 500 ×3 →
    failed with status code + body excerpt, 401/403 fail-fast (exactly one
    request, actionable message, `ErrAuth`-wrapping error), other 4xx not
    retried, connection-error retry, context cancellation before and in
    flight, trailing-`/` tolerance, and a leak check — the API key appears
    in no log line, error, reason, or request body. Retry waits run at
    millisecond scale via the injectable backoff field (production keeps
    1 s / 4 s / 16 s); the whole onyx suite runs in ~1 s.
- **T7 — CLI wiring** (`internal/cli/`, `cmd/sard/`)
  - stdlib `flag` per §4 (append-value helper for the repeatable
    `--include`/`--exclude`), `log/slog` logging; pipeline: resolve source →
    discover → transform → ingest → summary.
  - Accept: ✅ `--dry-run` prints one valid JSON payload per discovered file to
    stdout (stable order, deterministic IDs) and sends nothing; exit codes per
    §8 verified in `cli_test.go` — 0 (all OK, 0 files with a warning, successful
    dry-run), 1 (≥1 failed file; a 400 fails per-file without aborting), 2
    (missing API key / cc-pair-id, bad path, bad flag values including `--source`
    and non-int/negative ints, unknown subcommand, and a 401/403 auth abort),
    130 (SIGINT); end-to-end smoke tests invoke `Run` — the same entry function
    the binary uses — against a `net/http/httptest` mock with temp-dir fixtures,
    including a `file://` git-style ingest with git provenance (skipped without
    a git binary).
- **T8 — Summary & polish**
  - Clean summary output (counts, elapsed, failure list); `--limit`; progress line per file.
  - Accept: ✅ the recorded mock is the reference (no real Onyx instance is
    available): `cli_test.go` asserts, from captured stderr, the progress
    lines and the summary header (total, created, updated, skipped, failed,
    well-formed elapsed) for a successful run; the failure list in ingestion
    order; the skipped count and the 0-files warning's skipped attribute;
    dry-run stdout strictly JSON payloads with the summary on stderr; the
    `--limit` totals (ingest and dry run); and the warn-level interrupted
    summary (exit 130).
- **T9 — Test hardening**
  - Edge cases: CRLF, non-UTF-8, very long single file, monorepo depth, `.mdx` frontmatter.
  - Accept: ✅ full `go test ./...` green; coverage gate — `config` ≥ 90 %
    (93.5 %), `transform` ≥ 90 % (100 %), and each URL-normalization
    function in `internal/source/git.go` ≥ 90 % (all ten at 100 % after
    closing the two known gaps — the `url.Parse` error branch and the
    slash-less `stripGitSuffix` path). The gate is per function for the
    URL normalization, not for the source package as a whole (§4).
- **T10 — Build & README**
  - Build a single static binary: `CGO_ENABLED=0 go build -ldflags "-X main.version=…"
    -o sard ./cmd/sard`; README quickstart (prebuilt binary or `go install
    sardonyx/cmd/sard`), prerequisites (API key, CC-pair creation walkthrough), and the
    stale-document limitation.
  - Accept: ✅ verified from a clean tree: `go build ./…` succeeds;
    `CGO_ENABLED=0 go build -ldflags "-X main.version=1.0.0" -o sard
    ./cmd/sard` yields a static binary (`file sard` → "statically
    linked"); `./sard ingest ./internal/source/testdata/local
    --dry-run` exits 0 with one valid JSON payload per file, no
    network, and no Onyx credentials (a dry run needs none — the
    enabling config pre-flight change, decision in §4); both README
    examples (local directory and git URL) followed literally exit 0.
    No Go behavior changes beyond that enabling change; the coverage
    reading above is unchanged by T10 (config 93.6%, transform 100%,
    all ten normalization functions 100%).
- **T11 — `sard check`** (§13) — a second subcommand that verifies the
  environment without creating any document: configuration pre-flight,
  connectivity, credentials, and best-effort cc-pair validation.
  - T11a — `onyx.Client.Check` in a new `internal/onyx/check.go` (probes
    1/2/2f/3, the new `ErrUnreachable` / `ErrCCPairNotFound` sentinels,
    the `CheckResult` / `CCPairInfo` types).
    - Accept: ✅ `go test ./internal/onyx` green: the happy path (exactly one
    request per probe — method, path, and Bearer header asserted; the key
    in no returned string); 401 on probe 2 → `ErrAuth` with exactly one
    request (no retry, no further probes); 403 on probe 2 → the 2f
    disambiguation (the real run's endpoint decides): POST 403 →
    `ErrAuth`, POST 422 → `KeyOK` + `UsedFallback`, POST 200 → `KeyOK`
    (warning); GET 404/405 → 2f, whose request body is asserted to be
    exactly `{}` with `Content-Type: application/json`; 2f POST 404/405 →
    non-auth error (no Ingestion API at this URL); 429 → 200 (2 requests)
    and 500 ×3 → non-auth error (millisecond backoff injection); a
    connection failure on probe 1 → `ErrUnreachable` with *no* requests
    to the other endpoints; a connection failure on probe 2 after 3
    attempts → `ErrUnreachable`; health 200 with `success:false` and
    health 404 → no error, `Healthy=false`; probe 3 200 → `CCPair`
    populated; probe 3 404 after a GET success → `ErrCCPairNotFound`;
    probe 3 403/404 in the fallback flow → no error, `CCPair` nil;
    context cancellation mid-flight → the context's error. The suite
    runs in ~1 s.
  - T11b — the `sard check` command in `internal/cli` (flags, usage
    template, `runCheck`, the report, the exit-code mapping, package doc).
    - Accept: ✅ end-to-end `Run` against `httptest` — exit 0 (report lines on
    stderr, stdout empty, the server received no document-shaped POST);
    exit 2 for a missing API key / missing cc-pair-id, a negative
    `--cc-pair-id`, an invalid `--log-level`, a 401, a 403, a cc-pair 404
    (the message names the configured id, never the key), and a URL with
    no Ingestion API; exit 1 for a dead port; 130 for SIGINT mid-check;
    `sard check --help` → exit 0 with exactly four flags and an
    Environment section listing exactly `ONYX_API_URL`, `ONYX_API_KEY`,
    `ONYX_CC_PAIR_ID` (a `TestCheckUsageTemplateEnvNames` guard, mirroring
    the ingest one); the root usage lists `check` and `ingest`. The
    existing ingest/dry-run suite passes unmodified.
  - T11c — README ("Verify your setup" subsection in Getting started +
    an intro line) and the final docs pass.
    - Accept: ✅ the Getting started flow reads set credentials →
    `sard check` → first real run; `go test ./...` and `go vet ./...`
    green; the coverage gates unchanged (config ≥ 90 % — 93.6 % —,
    transform 100 %, all ten URL-normalization functions 100 %; the
    onyx and cli packages are not gated).

---

## 11. Open questions & risks

| # | Item | Current call |
| - | ---- | ------------ |
| 1 | **Stale documents**: Ingestion API has no delete → removed files persist in Onyx. | Accept for v1; document clearly; future: Onyx document-deletion API or periodic full prune if one exists. |
| 2 | **CC-pair prerequisite**: user must create a Connector (e.g. a File Connector) in the Admin Panel and read the `cc_pair_id` from the URL. | Documented — the README's Getting started carries the step-by-step walkthrough (T10); the prerequisite itself stays user-side. |
| 3 | **Cloud rate limits** on `cloud.onyx.app` for bulk ingests. | Sequential + backoff on 429; `--limit` for chunked runs. |
| 4 | **Tags**: `metadata` values become Onyx tags; free-form values may be noise in UI. | Keep metadata conservative (repo, path, commit); tags via future `--tag` flag. |
| 5 | **Branch/tag targeting**: `--branch` covers branches; tags are a small extension (git handles both in `--branch`). | Support via `--branch` (git accepts refs). |
| 6 | **Private repos without token in URL**: some users put the token in the URL itself. | Support `--token` injection; also pass through URLs that already embed a token. |
| 7 | **`source` enum for non-GitHub/GitLab hosts** (Bitbucket, self-hosted Gitea). | Default `file`; `--source` override available. |
| 8 | **Very large monorepos**: thousands of md files → long sequential runs. | `--include` scoping + `--limit`; concurrency is a v2 item. |
| 9 | **Go toolchain on the target machine**: only needed to *build*; the shipped binary is static. | Document build instructions; ship prebuilt binaries for common platforms. |
| 10 | **Document ID migration**: forks, repo URL changes, and moved local roots change the default ID base → new IDs → previous documents go stale (no delete API, #1). | `--id-base` / `SARD_ID_BASE` override; recommend pinning one canonical base per project (§6). |
| 11 | **The Ingestion API now lists a delete operation** (reference fetched 2026-10-02, "Delete Ingestion Doc"): the v1 premise that the API has no delete (#1, README limitation) is likely out of date. | v1 stance (no delete; stale documents documented) stands; verify the operation's auth and semantics before any v2 prune work. `sard check` (T11) does not depend on it. |
| 12 | **401/403 behind a reverse proxy / WAF**: a fronting proxy can return 401/403 for reasons other than key rejection; the check's advice assumes Onyx itself answered. | No v1 mitigation: the report carries the status code so a human can tell them apart; the hint still names the variable to check. |
| 13 | **Permission matrix beyond the plain API key**: the check targets the documented key shape (`manage:connectors` / `admin`); a Group Manager key 403s on the *list* endpoint yet may still ingest into the pairs of the groups it manages. | A probe-2 403 falls through to the POST fallback — the endpoint a real run uses — and its verdict wins; probe 3 treats 403/404 (fallback flow) as warnings, not failures. Real runs have the same limitation today (§2). |

---

## 12. Testing strategy

- **Unit (no network):** `*_test.go` next to each package — URL normalization;
  ID stability/uniqueness; transform mapping; local-dir walking on `testdata/` fixture trees;
  config precedence.
- **HTTP integration:** an in-test `net/http/httptest` server (stdlib — no external mock
  library) recording requests; verify payload shape, retry behavior, and `already_existed`
  mapping.
- **Git integration:** clone a fixture repo built in a temp dir via a local path (a repo
  under `testdata/` would nest a repo in a repo); verify `git ls-files`-based discovery and
  commit metadata extraction (git binary required — document as a runtime dependency; tests
  `t.Skip` when git is not on PATH).
- **End-to-end smoke:** `internal/cli` runs the full pipeline with `--dry-run` against the
  `httptest` server, invoked from the same entry function the binary uses.

---

## 13. The `sard check` command (T11)

**Status:** implemented — T11 (§10) is done: `onyx.Client.Check`
(`internal/onyx/check.go`), the `sard check` command (`internal/cli`),
and the README's "Verify your setup" section.

`sard check` is a second subcommand (alongside `ingest`) that verifies the
environment **without creating, updating, or deleting any document**:

1. **Configuration** — the exact pre-flight a real run performs:
   `ONYX_API_KEY` and `ONYX_CC_PAIR_ID` required (flag > env > `.env`),
   invalid values rejected (exit 2).
2. **Connectivity** — the Onyx server at `--api-url` answers.
3. **Credentials** — the API key is accepted **with the permissions the
   Ingestion API requires** (`manage:connectors` or `admin`), not merely
   *some* permission.
4. **cc-pair (best-effort)** — the configured `cc_pair_id` exists on the
   deployment; the check reports the connector name, the pair's status,
   and the number of indexed documents. A non-existent id is an
   **error**: the Ingestion `POST` accepts a bogus id (the documents are
   created but never appear on the Connectors page), so a real run with
   a wrong id fails silently.

The purpose: it is *the* pre-flight for a real run. After setting up
credentials, run `sard check` once; when it exits 0, the first
`sard ingest` cannot fail on an environmental problem.

### 13.1 Spec

```
sard check [options]                (no positional arguments)

Options
  --api-url      Onyx API base URL        (default https://cloud.onyx.app/api; env ONYX_API_URL)
  --api-key      Onyx API key             (env ONYX_API_KEY)
  --cc-pair-id   Onyx connector-credential-pair id, int  (env ONYX_CC_PAIR_ID)
  --log-level    debug | info | warning | error (default info)
```

Only the Onyx-side flags exist — no `--source`, `--branch`, `--token`,
include/exclude, depth/size, `--limit`, `--dry-run`, or `--id-base`:
discovery is out of scope for check, and git concerns do not apply.
`--cc-pair-id` is treated exactly as in ingest: `0` is "unset", negative
is invalid. The usage template follows the ingest convention (a Long
summary; an Environment section listing exactly `ONYX_API_URL`,
`ONYX_API_KEY`, `ONYX_CC_PAIR_ID`; no Arguments section, since there is
no positional argument).

### 13.2 The probes

Three HTTP probes, in order. Every probe is read-only from Onyx's point
of view — the one request that resembles a write is the
deliberately-invalid fallback below, §13.3.

| #   | Request                                                  | Auth   | Purpose                                                                  | Failure handling |
| --- | -------------------------------------------------------- | ------ | ------------------------------------------------------------------------ | ---------------- |
| 1   | `GET {api-url}/health`                                   | none   | Reachability; the body's `success` field. Single attempt, 10 s deadline. | Connection failure → **stop the check, exit 1** (no point probing further). 404/405 (older deployment without the endpoint) or any other non-200 → **warn and continue**: probe 2 doubles as the reachability test. |
| 2   | `GET {api-url}/onyx-api/ingestion`                        | Bearer | Credentials (primary path) + a document count: the read-only sibling of the `POST` a real run uses — same key, same `manage:connectors`/`admin` requirement, so a 200 is specifically a statement about the key's ingestion ability. | 200 → key OK (count reported). 401 → **exit 2** (bad/expired key; the `authHint` advice). 403 → **to 2f for disambiguation** — the key may simply lack the permission for *this* list endpoint while still being able to ingest (e.g. a Group Manager); the POST is the endpoint a real run uses, so its verdict wins. 404/405 (deployment predates the GET) → 2f. 429/5xx/connection → up to 3 attempts with the injectable backoff (same machinery as `Ingest`); exhausted → exit 1 (`ErrUnreachable` for connection, or a persistent server error). |
| 2f  | Fallback: `POST {api-url}/onyx-api/ingestion` with body `{}` | Bearer | Credentials, checked against the real endpoint (older deployments; or after a probe-2 403). | 401/403 → **exit 2** with the `authHint` advice (now against the endpoint a real run uses). 400/422 → the endpoint exists, auth passed, the body was rejected → **key accepted** (`UsedFallback`). Any other 4xx (404, 405, …) → **exit 2**: "no Onyx Ingestion API at this URL — check `ONYX_API_URL`" (catches a misconfigured base, e.g. a missing trailing `/api`). An unexpected 200 → the key is accepted (the verifiable fact) with a warning. 429/5xx/connection → retried as above; exhausted → exit 1. |
| 3   | `GET {api-url}/manage/admin/cc-pair/{id}`                 | Bearer | cc-pair validation: name, status, `num_docs_indexed`. | Runs whenever the key was accepted. 200 → the cc-pair report. 404 → **exit 2** "cc-pair-id N not found on this deployment — copy it from the connector's Admin Panel URL", **only when probe 2 was the GET** (the deployment is modern, so the endpoint is known to exist and the 404 means the id doesn't); in the fallback flow an absent endpoint is indistinguishable from an absent pair → warn, don't fail. 403 → warn and continue (the key lacks the scope for *this* endpoint — a Group Manager outside its groups — while having passed the probe that matters for ingestion). Any other outcome → warn (best-effort probe), continue. |

Each probe runs under a 10 s deadline (`context` timeout, inside the
client's 30 s `http.Client` backstop). Probes 2 and 3 run only if probe 1
did not find the server unreachable. The whole check runs under a
signal-aware context (Ctrl-C → exit 130, as with ingest).

### 13.3 Why a document can never be created

The only write path Sardonyx uses in Onyx is `POST /onyx-api/ingestion`,
and the fallback (2f) does use that path — so the plan must show the
probe cannot create a document:

- The body is `{}`. The request schema requires the `document` object
  (`cc_pair_id` is nullable in the current OpenAPI), so the body fails
  validation — no handler logic runs, nothing is written.
- On FastAPI (Onyx's stack) the Bearer-auth security dependency is
  evaluated before the request body is parsed, and the Ingestion API
  guide documents a bad key as a 4xx error response — so a rejected key
  never reaches body validation, and an accepted key is rejected by the
  schema before any code that could create a document runs.
- Probes 2 (primary), 3, and 1 are plain GETs (probe 1 needs no auth at
  all).

Residual assumption (stated in the output when it applies): a deployment
that validated the body *before* auth (non-standard ordering) could
return 422 to a bad key, which the fallback would misread as "key
accepted". The fallback runs only on deployments without the GET
endpoint, and the report explicitly marks when it was used, so the
verdict can be read with that caveat.

### 13.4 Exit codes

| Code | Condition (check)                                                                                                                                  |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0    | Server reachable, key accepted (probe 2 or 2f), cc-pair validated — or, where the cc-pair probe could not run, reported as a warning.               |
| 1    | Server unreachable (connection failure/timeout — probe 1, or probe 2/2f after retries) or persistent 429/5xx.                                       |
| 2    | Configuration error — the same pre-flight as a real run (missing/invalid `ONYX_API_KEY`/`ONYX_CC_PAIR_ID`, negative `--cc-pair-id`, invalid `--log-level`) — or a rejected key (401/403: a credentials problem, the same classification as the ingest fail-fast, §8) — or the configured cc-pair-id not present on the deployment (probe 3, 404) — or no Onyx Ingestion API at the configured URL (2f, other 4xx). |
| 130  | Interrupted (Ctrl-C).                                                                                                                                                                        |

The stdout/stderr discipline is inherited from §8: all diagnostics on
stderr; stdout stays empty (check prints no JSON).

### 13.5 Report

One info line per probe, then a summary header (house style, §8) — all on
stderr:

```
time=... level=INFO msg="Onyx at https://cloud.onyx.app/api: health ok"
time=... level=INFO msg="API key accepted — 23 documents visible via the ingestion API"
time=... level=INFO msg="cc-pair 243: My Docs — ACTIVE, 12 documents indexed"
time=... level=INFO msg="check complete: reachable, key ok, cc-pair verified in 1.84s"
```

A failed check prints no summary header — a single actionable error
line, then the exit code (the same shape as the ingest auth fail-fast).
A run that used the fallback (2f) adds a note to its key line (the
caveat of §13.3, visible in the output).

### 13.6 Implementation

**`internal/onyx/check.go`** (new file beside `client.go`; `client.go`
keeps `Ingest` as-is):

- `func (c *Client) Check(ctx context.Context) (CheckResult, error)` —
  runs probes 1 → 2 (or 2f) → 3 per §13.2.
- Types:

```go
type CCPairInfo struct {
	ID     int
	Name   string
	Status string
	Docs   int
}

type CheckResult struct {
	Healthy      bool        // probe 1: 200 with success=true
	KeyOK        bool        // probe 2 or 2f accepted the key
	DocCount     int         // documents visible to the key (probe 2 only)
	CCPair       *CCPairInfo // nil = not validated (the report warns)
	UsedFallback bool        // true = probe 2f (the invalid POST) was used
}
```

- New sentinels (beside `ErrAuth` in the package): `ErrUnreachable`
  (connection failure/timeout, or persistent 429/5xx — after retries)
  and `ErrCCPairNotFound` (probe 3, 404, modern deployment). `Check`
  wraps them; the CLI maps them via `errors.Is` to exit 1 / 2.
- Retryable classes (429, 5xx, connection) reuse the existing
  injectable-backoff machinery from `Ingest` for probes 2/2f; probe 1 is
  a single un-retried attempt, and probe 3 is not retried either (a 429
  there just downgrades to a warning — its verdict is best-effort).
- The API key never appears in any error or reason string (the existing
  discipline; the leak test is extended to `Check`).

**`internal/cli/cli.go`**:

- New `checkFlags{apiURL, apiKey, ccPairID, logLevel}` struct (separate
  from `ingestFlags` — check has no git or source flags), `newCheckCmd`
  (`Use: "check"`, `Args: cobra.NoArgs`, the four flags above,
  `checkUsageTemplate` = the ingest template minus the Arguments
  section, Environment section listing exactly the three Onyx
  variables), and `runCheck` mirroring `runIngest`'s pre-flight shape:
  `buildLogger` → flag validation (`--cc-pair-id >= 0`) →
  `config.Resolve(Flags{…, DryRun: false})` → `signal.NotifyContext` →
  `onyx.NewClient(…).Check(ctx)` → the report → the exit code.
- `Run` adds the command to the root; the root usage template already
  renders the subcommand list, so no template change is needed (the
  existing root-usage tests gain an assertion for `check`).
- The package doc is updated to describe the second subcommand and the
  `check` exit-code mapping.
- **`internal/config` is unchanged**: `check` calls `Resolve` with
  `DryRun: false` — exactly the required-credentials set a real run has.
  That is the point of the command: check is the pre-flight of a real
  run, minus the documents.

**Tests** (`internal/onyx/check_test.go`, additions to
`internal/cli/cli_test.go`): see the acceptance criteria in §10.

**README** (T11c): a new "Verify your setup" subsection in Getting
started (between the dry-run step and the real run: set credentials →
`sard check` → first real run), the failure modes (bad key, unknown
cc-pair, wrong URL), and a line in the intro example block.
