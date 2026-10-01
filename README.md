# sardonyx

Ingest **markdown files** from a **git repository URL** or a **local directory** into
[Onyx](https://onyx.app) via the [Ingestion API](https://docs.onyx.app/developers/guides/index_files_ingestion_api).

> **Status:** 🚧 In implementation — the design lives in [`docs/PLAN.md`](docs/PLAN.md).
> Tasks T1–T7 (config, models, transform, local directory source, git
> repository source, Onyx client, CLI wiring) are complete; T8–T10 are
> still to do (summary polish, test hardening, build & README).
> See the task list in `docs/PLAN.md` §10.

## Why the name

**Sardonyx** is a red-and-black banded variety of onyx (both are forms of
chalcedony) — the repo and Go module carry the full name, while the
command line tool is simply **`sard`**, the stone's own name: short, easy to
type, and unambiguous as a CLI. It builds to a single static binary: no
interpreter or virtualenv needed on the target machine.

## What it will do

```bash
sard ingest https://github.com/owner/repo            # all *.md / *.mdx / *.markdown in the repo
sard ingest ./my-docs                                # all markdown files under a local directory
sard ingest https://github.com/owner/repo --include "docs/**" --dry-run
```

- Discovers markdown files (`.md`, `.mdx`, `.markdown`) in a cloned repo (shallow clone,
  tracked files only) or a local directory tree.
- Converts each file to an Onyx `IngestionDocument`:
  - stable, **deterministic document IDs** → re-running updates existing documents
    instead of duplicating them (upsert semantics, `already_existed`),
  - `semantic_identifier` like `owner/repo/path/to/file.md`,
  - title extracted from the first `#` heading (falls back to the filename),
  - section `link` pointing at the GitHub blob URL where applicable,
  - `doc_updated_at` from the last commit (git) or file mtime (local),
  - metadata tags: repo, path, commit SHA, and `ingested_by: sardonyx`.
- Sends each document to `POST {API_BASE_URL}/onyx-api/ingestion` with retry/backoff,
  then prints a summary (created / updated / skipped / failed) and an exit code.

## Prerequisites (Onyx side)

1. An **Onyx API key** with the `manage:connectors` permission (or `admin` role).
2. A **Connector / CC-pair**: create one in the Onyx Admin Panel (e.g. a File Connector)
   and copy its `cc_pair_id` from the URL (`/admin/connector/<cc_pair_id>`).
   Documents ingested with that `cc_pair_id` appear on the Connectors page.

## Configuration

Priority: CLI flag → environment variable → `.env` file.

| Env var           | Purpose                                   | Default                     |
| ----------------- | ----------------------------------------- | --------------------------- |
| `ONYX_API_URL`    | Onyx API base URL                         | `https://cloud.onyx.app/api`|
| `ONYX_API_KEY`    | Bearer API key                            | *(required)*                |
| `ONYX_CC_PAIR_ID` | Connector-credential pair id for the docs | *(required for the UI)*     |
| `GIT_TOKEN`       | Token for private repos (injected into the clone URL) | *(optional)*   |
| `SARD_ID_BASE`    | Arbitrary document-ID base for the run (affects the ID only; PLAN §6) | *(optional)* |

See [`.env.example`](.env.example).

## Repository layout

```
docs/
  PLAN.md                # full implementation plan + task breakdown (T1–T10)
  onyx-ingestion-api.md  # condensed reference for the Onyx Ingestion API
cmd/sard/
  main.go                # thin entry point: run + exit code        (T7)
internal/
  cli/cli.go             # flags, pipeline orchestration, summary   (T7, T8)
  config/config.go       # flags > env > .env resolution            (T1)
  models/models.go       # IngestedFile / IngestResult / OnyxPayload (T2)
  source/git.go          # URL normalization, shallow clone, git ls-files (T5)
  source/local.go        # directory walk, filters, mtime           (T4)
  transform/markdown.go  # IngestedFile → Onyx payload              (T3)
  onyx/client.go         # HTTP client, retries, result mapping     (T6)
```

Tests live next to the code in each package as `*_test.go` (mock Onyx server
via `net/http/httptest`); git fixtures live under `internal/source/testdata/`.

## Known limitations (v1)

- **No deletion**: the Ingestion API cannot remove documents, so files deleted from the
  source keep their (stale) Onyx documents. Re-ingesting updates them, but deletion is a
  gap — see `docs/PLAN.md` §11.
- One document = one markdown file, one section (no per-heading chunk splitting yet).
- Sequential ingestion only (no parallel uploads in v1).

## Docs & references

- [Implementation plan](docs/PLAN.md) — architecture, CLI spec, ID strategy, task list.
- [Onyx Ingestion API notes](docs/onyx-ingestion-api.md).
- Official guide: <https://docs.onyx.app/developers/guides/index_files_ingestion_api>
- Onyx core concepts: <https://docs.onyx.app/developers/core_concepts>
