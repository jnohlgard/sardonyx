# sardonyx — Implementation Plan

**Status:** 🚧 Planning — no implementation yet. This document is the blueprint for the
implementation tasks in §10.

---

## 1. Goal

A small Python CLI tool that:

1. Takes **one positional input**: a git repository URL **or** a local directory path.
2. Discovers all **markdown files** (`*.md`, `*.mdx`, `*.markdown`).
3. Transforms each file into a valid Onyx **Ingestion API** payload.
4. POSTs the payloads to the Onyx Ingestion API so the documents appear in the Onyx
   workspace (under a designated Connector / CC-pair).

### Requirements

| ID   | Requirement                                                                                                                                                          |
| ---- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R1   | Accept a git repo URL (`https://`, `git@` ssh form, or `owner/repo` shorthand) or a local filesystem path as the single positional input.                                                            |
| R2   | Discover markdown files: `.md`, `.mdx`, `.markdown`.                                                                                                                 |
| R3   | Produce valid Onyx `IngestionDocument` payloads (see §6).                                                                                                            |
| R4   | Send payloads to the Ingestion API with Bearer auth, timeouts, and retries (§7).                                                                                    |
| R5   | **Idempotent re-runs**: deterministic, stable document IDs so re-ingesting the same repo/directory **updates** existing documents instead of duplicating.          |
| R6   | Per-file error isolation: one bad file does not abort the run; print a final summary (new / updated / failed); meaningful exit codes (§8).                         |
| R7   | `--dry-run`: print the payloads that would be sent, send nothing.                                                                                                    |
| R8   | Configuration via CLI flags → environment variables → `.env` file, in that priority order (§5.1).                                                                    |
| R9   | Default exclusion of noise dirs (`.git`, `node_modules`, `dist`, `build`, `vendor`, `.venv`, …) plus `--include` / `--exclude` glob overrides and `--max-depth`.  |
| R10  | Test suite covering transform logic, ID stability, and HTTP behavior against a mocked Onyx server.                                                                   |

### Non-goals (v1)

- **Deleting/pruning** documents from Onyx — the Ingestion API has no delete endpoint, so
  deleted source files leave stale documents behind. Mitigation is metadata/naming; a future
  version may call a separate Onyx deletion API if one exists.
- Non-markdown file types (PDF, DOCX, …).
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
                    │  git repo /      │    │  markdown →       │    │  HTTP + retry  │
                    │  local directory │    │  IngestionDoc     │    │  + summary     │
                    └────────┬─────────┘    └───────────────────┘    └────────────────┘
                             │
                    ┌────────┴─────────┐
                    │ 0. Config        │  CLI flags > env vars > .env
                    └──────────────────┘
```

- **Source layer** (one module per input kind) answers: *"which files exist, what is their
  content, and what are their provenance facts?"* It emits a list of `IngestedFile` records.
- **Transform layer** maps each `IngestedFile` → an Onyx payload dict (pure function, no I/O).
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
  --include GLOB   Include filter, repeatable (e.g. "docs/**", "*.mdx")
  --exclude GLOB   Exclude filter, repeatable (on top of the default noise-dir exclusions)
  --max-depth N    Max directory depth below the source root
  --max-file-size  Skip files larger than N KiB (default 1024)
  --token          Git auth token for private repos (env GIT_TOKEN); injected as x-access-token in the HTTPS URL
  --dry-run        Print payloads to stdout; send nothing
  --limit N        Ingest at most N files (smoke-testing)
  --log-level      debug | info | warning | error (default info)
```

Examples (to be documented in README once implemented):

```bash
sard ingest https://github.com/onyx-dot-app/onyx --cc-pair-id 42
sard ingest ./my-docs --api-url http://onyx.local:8080/api --api-key $ONYX_API_KEY
sard ingest https://github.com/org/repo --include "docs/**" --dry-run
```

---

## 5. Source layer

### 5.1 Configuration resolution

Priority: **CLI flag > environment variable > `.env` file (project root) > default**.
Env vars: `ONYX_API_URL`, `ONYX_API_KEY`, `ONYX_CC_PAIR_ID`, `GIT_TOKEN`.
`.env` loading via `python-dotenv`; a missing `.env` is not an error.

### 5.2 Git repository source

- **URL normalization** (pure function, unit-tested):
  - `https://host/owner/repo[.git]` → unchanged (token-injection point preserved).
  - `git@host:owner/repo[.git]` → `https://host/owner/repo` (we never clone over SSH).
  - `owner/repo` shorthand → `https://github.com/owner/repo`.
  - Host detection → default `--source` value: `github.com` → `github`, `gitlab.*` → `gitlab`,
    anything else → `file`.
- **Cloning:** `git clone --depth 1 [--branch <b>] <url> <tmpdir>` into a fresh temp dir
  (shallow = fast, small); temp dir removed on exit (even on error).
- **Private repos:** when `--token`/`GIT_TOKEN` is set, clone URL becomes
  `https://x-access-token:<token>@host/owner/repo` (token never logged or echoed).
- **File discovery:** use `git ls-files '*.md' '*.mdx' '*.markdown'` against the clone.
  This lists **tracked** files only, so the repo's own `.gitignore` is respected for free —
  no manual noise-dir walking needed for this path.
- **Provenance per file:** `git log -1 --format=%H%x00%ct -- <path>` → commit SHA (→
  `metadata.commit`) and commit unix timestamp (→ `doc_updated_at`). One `git log` call per
  file is acceptable for v1 (batching is a future optimization).
- **Section link:** for `https://github.com/…` (public) URLs, blob URL
  `https://github.com/owner/repo/blob/<branch>/<path>`; otherwise `null`.

### 5.3 Local directory source

- Validate the path exists and is a directory (else config error, exit code 2).
- If the directory (or an ancestor) is a git repo, prefer `git ls-files` for the same
  reasons as §5.2 (and get commit metadata for free). Otherwise walk with `pathlib`.
- **Default exclusions** (walk mode only): `.git`, `node_modules`, `dist`, `build`,
  `vendor`, `.venv`, `__pycache__`, `.idea`, `.github` (keep README-adjacent dirs? —
  decision: include `.github` workflows dir? No: exclude; `--include` can override).
- `--max-depth` and `--include`/`--exclude` globs (matched against the path relative to the
  source root; `fnmatch`-style).
- Provenance: `doc_updated_at` = file mtime (UTC ISO-8601); `metadata.path` = relative path.
- Content read as UTF-8 with `errors="replace"`; undecodable/binary garbage is skipped with
  a warning.

### 5.4 Common output record

```python
@dataclass
class IngestedFile:
    kind: str                  # "git" | "local"
    root_label: str            # "owner/repo" or basename(abs(dir))
    relpath: str               # relative to the source root, posix separators
    content: str               # markdown text
    doc_updated_at: datetime   # commit time (git) or mtime (local)
    commit_sha: str | None
    blob_url: str | None
```

---

## 6. Transform: markdown → Onyx document

Pure function `to_onyx_payload(file: IngestedFile, source: str, cc_pair_id: int) -> dict`.

| Field                | Value (git)                                              | Value (local)                                  |
| -------------------- | -------------------------------------------------------- | ---------------------------------------------- |
| `id`                 | `sha256("git\0" + normalized_origin + "\0" + relpath)`   | `sha256("local\0" + normalized_abs_path)`      |
| `semantic_identifier`| `owner/repo/path/to/file.md`                             | `<dir-name>/path/to/file.md`                   |
| `title`              | first `# …` heading in the file, else the filename       | same                                           |
| `sections`           | `[{"text": content, "link": blob_url or None}]`          | `[{"text": content}]`                          |
| `source`             | `github` / `gitlab` / `file` (per §5.2; `--source` override) | `file` (or override)                   |
| `metadata`           | `{repo: <origin>, path: <relpath>, commit: <sha>, ingested_by: "sardonyx"}` | `{path: <relpath>, ingested_by: "sardonyx"}` |
| `doc_updated_at`     | last-commit timestamp, ISO-8601 UTC                      | mtime, ISO-8601 UTC                            |
| `from_ingestion_api` | `true`                                                   | `true`                                         |

- **ID design note:** branch is deliberately *not* part of the git ID — re-ingesting a
  different branch updates the same documents (last write wins). Two different branches
  ingested into the same CC-pair will overwrite each other; this is acceptable and documented.
- **Intentionally omitted:** `chunk_count` (let Onyx compute), `primary_owners` /
  `secondary_owners`, `additional_info`, image sections.
- **Content:** raw markdown text in a single section. v2 candidate: split into sections per
  top-level heading (better citations/links) — noted as a future task, not planned now.
- Frontmatter (YAML at top of `.mdx`/Jekyll files) is kept as-is in v1; stripping is a
  future option.

---

## 7. Onyx client

`OnyxClient(api_url, api_key, cc_pair_id)`:

- `ingest(payload) -> IngestResult` — one POST per document to `{api_url}/onyx-api/ingestion`.
  - Headers: `Authorization: Bearer <key>`, `Content-Type: application/json`; timeout 30 s.
  - **Retries:** up to 3 attempts with exponential backoff (1 s, 4 s, 16 s) on
    `429`, `5xx`, and connection/timeout errors. Never retry other 4xx.
  - **Fail-fast:** `401`/`403` → raise immediately with an actionable message
    ("check ONYX_API_KEY and that it has `manage:connectors` or `admin`").
  - **Result mapping:** `200` → `created` if `already_existed` is falsy, `updated` otherwise;
    non-200 after retries → `failed` with status code + body excerpt.
- Never logs the API key.

---

## 8. Error handling & exit codes

| Code | Meaning                                                             |
| ---- | ------------------------------------------------------------------- |
| 0    | Run completed; all discovered files ingested successfully (or 0 files found, with a warning) |
| 1    | Run completed but ≥1 file failed to ingest                          |
| 2    | Configuration error (missing api key / cc_pair_id, bad path, bad flags) |
| 130  | Interrupted (Ctrl-C)                                                |

- Per-file failures are logged, recorded in the summary, and do not abort the run.
- Summary printed at the end: counts of `created`, `updated`, `skipped` (size/empty),
  `failed` (with file + reason), plus total file count and elapsed time.

---

## 9. Repository layout

```
sardonyx/
├── README.md                      # project overview, setup, usage (filled in as tasks land)
├── pyproject.toml                 # packaging + deps + console-script entry point
├── .env.example                   # ONYX_API_KEY / ONYX_API_URL / ONYX_CC_PAIR_ID / GIT_TOKEN
├── .gitignore
├── docs/
│   ├── PLAN.md                    # this file
│   └── onyx-ingestion-api.md      # condensed reference for the Ingestion API
├── src/
│   └── sardonyx/
│       ├── __init__.py            # package docstring, __version__
│       ├── __main__.py            # `python -m sardonyx`
│       ├── cli.py                 # argparse, pipeline orchestration, summary
│       ├── config.py              # flags > env > .env resolution
│       ├── models.py              # IngestedFile, IngestResult, payload types
│       ├── sources/
│       │   ├── __init__.py
│       │   ├── git_repo.py        # normalize URL, clone, git ls-files, commit metadata
│       │   └── local_dir.py       # walk / git ls-files, filters, mtime
│       ├── transform/
│       │   ├── __init__.py
│       │   └── markdown.py        # IngestedFile → Onyx payload dict
│       └── client/
│           ├── __init__.py
│           └── onyx.py            # HTTP, retries, result mapping
└── tests/
    ├── __init__.py
    ├── test_url_normalize.py
    ├── test_doc_id_stability.py
    ├── test_transform.py
    ├── test_local_dir.py
    └── test_onyx_client.py        # against a mock HTTP server
```

---

## 10. Implementation tasks (in order)

Each task should land in its own commit with passing tests where applicable.

- **T1 — Config & environment** (`config.py`)
  - Implement flag > env > `.env` resolution; dataclass `Settings`.
  - Accept: ✅ unit tests for precedence; missing key detected pre-flight (exit 2).
- **T2 — Models** (`models.py`)
  - `IngestedFile`, `IngestResult` dataclasses; payload type alias.
  - Accept: ✅ importable, used by later modules.
- **T3 — Transform** (`transform/markdown.py`)
  - `to_onyx_payload()` exactly per §6 (id hashing, title-from-heading, metadata, timestamps).
  - Accept: ✅ unit tests: deterministic ID across two calls; branch not in ID; local vs git
    shapes; first-heading title; no-heading fallback; ISO-8601 UTC formatting.
- **T4 — Local directory source** (`sources/local_dir.py`)
  - Walk + default exclusions + include/exclude globs + max-depth + max-file-size + mtime.
  - Accept: ✅ tests with a fixture tree (incl. nested noise dirs); empty-file skip.
- **T5 — Git repo source** (`sources/git_repo.py`)
  - URL normalization, shallow clone (temp dir), `git ls-files`, per-file `git log` metadata,
    token injection, blob URLs, cleanup.
  - Accept: ✅ unit tests for URL normalization (pure fn); integration test that clones a
    small local fixture repo (`file://` or a local path) and yields expected files + metadata.
- **T6 — Onyx client** (`client/onyx.py`)
  - POST + auth + timeout + retries + fail-fast + result mapping (per §7).
  - Accept: ✅ tests against a mock server: success (new/updated), 429 then 200, 500 x3 →
    failed, 401 fail-fast, key never in logs.
- **T7 — CLI wiring** (`cli.py`, `__main__.py`)
  - argparse per §4; pipeline: resolve source → discover → transform → ingest → summary.
  - Accept: ✅ `--dry-run` prints valid JSON payloads; exit codes per §8; end-to-end smoke
    test against the mock server.
- **T8 — Summary & polish**
  - Clean summary output (counts, elapsed, failure list); `--limit`; progress line per file.
  - Accept: ✅ manual run against a real Onyx instance (or recorded mock) matches expectations.
- **T9 — Test hardening**
  - Edge cases: CRLF, non-UTF-8, very long single file, monorepo depth, `.mdx` frontmatter.
  - Accept: ✅ full `pytest` green; coverage of the pure functions ≥ 90 %.
- **T10 — Packaging & README**
  - Verify `pip install -e .` → `sardonyx` console script works; README quickstart
    with real examples, prerequisites (API key, CC-pair creation walkthrough), and the
    stale-document limitation.
  - Accept: ✅ fresh venv install + dry-run works from the README instructions.

---

## 11. Open questions & risks

| # | Item | Current call |
| - | ---- | ------------ |
| 1 | **Stale documents**: Ingestion API has no delete → removed files persist in Onyx. | Accept for v1; document clearly; future: Onyx document-deletion API or periodic full prune if one exists. |
| 2 | **CC-pair prerequisite**: user must create a Connector (e.g. a File Connector) in the Admin Panel and read the `cc_pair_id` from the URL. | Document step-by-step in README (task T10). |
| 3 | **Cloud rate limits** on `cloud.onyx.app` for bulk ingests. | Sequential + backoff on 429; `--limit` for chunked runs. |
| 4 | **Tags**: `metadata` values become Onyx tags; free-form values may be noise in UI. | Keep metadata conservative (repo, path, commit); tags via future `--tag` flag. |
| 5 | **Branch/tag targeting**: `--branch` covers branches; tags are a small extension (git handles both in `--branch`). | Support via `--branch` (git accepts refs). |
| 6 | **Private repos without token in URL**: some users put the token in the URL itself. | Support `--token` injection; also pass through URLs that already embed a token. |
| 7 | **`source` enum for non-GitHub/GitLab hosts** (Bitbucket, self-hosted Gitea). | Default `file`; `--source` override available. |
| 8 | **Very large monorepos**: thousands of md files → long sequential runs. | `--include` scoping + `--limit`; concurrency is a v2 item. |

---

## 12. Testing strategy

- **Unit (no network):** URL normalization; ID stability/uniqueness; transform mapping;
  local-dir walking on fixture trees; config precedence.
- **HTTP integration:** a local mock Onyx server (e.g. `http.server` in a fixture, or
  `pytest-httpx`/`respx`) recording requests; verify payload shape, retry behavior, and
  `already_existed` mapping.
- **Git integration:** clone a small local fixture repo via `file://`/local-path clone;
  verify `git ls-files`-based discovery and commit metadata extraction (git binary required —
  document as a runtime dependency).
- **End-to-end smoke:** `--dry-run` against the mock server from the CLI entry point.
