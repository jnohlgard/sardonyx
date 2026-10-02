# Sardonyx

Ingest **Markdown files** from a **git repository URL** or a **local directory** into
[Onyx](https://onyx.app) via the [Ingestion API](https://docs.onyx.app/developers/guides/index_files_ingestion_api).

> **Status:** ✅ Complete — all implementation tasks T1–T10 are done (task list in
> `docs/PLAN.md` §10). The tool builds to a single static binary
> (see [Building](#building)).

## What it does

```bash
sard ingest https://github.com/owner/repo            # all *.md / *.mdx / *.markdown in the repo
sard ingest ./my-docs                                # all Markdown files under a local directory
sard ingest https://github.com/owner/repo --include "docs/**" --dry-run
```

- Discovers Markdown files (`.md`, `.mdx`, `.markdown`) in a cloned repo (shallow clone,
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

## Getting started

### 1. Prerequisites (Onyx side)

You need an Onyx instance (cloud or self-hosted) and two things in it:

1. **An API key with the right permission.** Create an API key in the Onyx
   Admin Panel (cloud: `cloud.onyx.app/admin`; self-hosted: your deployment's
   Admin Panel — ask your deployment admin if you cannot create keys
   yourself) and make sure it carries the **`manage:connectors`** permission
   or an **admin** role — that is what the
   [Ingestion API](https://docs.onyx.app/developers/guides/index_files_ingestion_api)
   requires. (A Group Manager may ingest into the CC-pairs of the groups
   they manage, but not into the default public pair.)
2. **A Connector / CC-pair to receive the documents.**
   1. In the Onyx **Admin Panel**, open the **Connectors** list and create
      a new connector — a **File Connector** works for plain Markdown files.
   2. Open the created connector and copy its `cc_pair_id` from the URL:
      `https://cloud.onyx.app/admin/connector/243` → `cc_pair_id = 243`.
   3. Documents ingested with that `cc_pair_id` appear on the Connectors
      page under that connector.

### 2. Get `sard`

There is no release pipeline yet, so **build from source**. One static
binary comes out — the target machine needs no Go toolchain, interpreter, or
virtualenv.

```bash
git clone https://github.com/jnohlgard/sardonyx
cd sardonyx
CGO_ENABLED=0 go build -ldflags "-X main.version=1.0.0" -o sard ./cmd/sard
```

- `CGO_ENABLED=0` makes the binary fully static (`file sard` →
  `… statically linked …`), so it runs on almost any Linux machine.
- The `-X main.version=…` stamp is optional (it defaults to `dev`); it is
  what `sard --version` prints.
- Building needs a Go toolchain (`go 1.24` per `go.mod`) and, for ingesting
  git repositories at run time, a `git` binary on the target.

Alternative for your own machine: `go install` from the checkout —

```bash
go install sardonyx/cmd/sard   # lands in $GOBIN (default ~/go/bin)
```

### 3. First run — dry run (no Onyx credentials needed)

`--dry-run` prints the exact payload that *would* be sent to Onyx — one JSON
document per line, in stable file order — and sends nothing. It needs **no
API key and no cc-pair id** (the printed `cc_pair_id` is `0` until you
provide one), so it works on a machine with zero Onyx configuration:

```bash
sard ingest https://github.com/owner/repo --dry-run   # from a git repository
sard ingest ./my-docs --dry-run                       # from a local directory
```

Inspect the output to confirm discovery and the document shape before you
spend a real run on it.

> **⚠️ Read this first — stale documents.** The Ingestion API has **no delete
> endpoint**: a file removed from the source leaves its (stale) document
> behind in Onyx. Re-running `sard` updates existing documents (upsert by
> stable ID) but never removes anything. Ingest only the directories you
> intend to keep; see [Known limitations](#known-limitations-v1) and
> `docs/PLAN.md` §11 for the discussion.

### 4. Real run

Put your credentials where you like — CLI flag, environment variable, or a
`.env` file in the working directory (precedence: flag → env → `.env`);
see [.env.example](.env.example):

```bash
cp .env.example .env      # then fill in ONYX_API_KEY and ONYX_CC_PAIR_ID
sard ingest https://github.com/owner/repo
sard ingest ./my-docs
```

The first run **creates** the documents; every later run **updates** them
(same stable IDs). The end-of-run summary on stderr reports
`created / updated / skipped / failed` and an exit code: `0` all good,
`1` at least one file failed, `2` configuration/credentials error,
`130` interrupted.

## Building

The standard build recipe (static binary, version-stamped):

```bash
CGO_ENABLED=0 go build -ldflags "-X main.version=1.0.0" -o sard ./cmd/sard
```

- `CGO_ENABLED=0` → single static binary, no shared-library dependencies.
- `-X main.version=<version>` → stamps the build's version string
  (default `dev` when omitted); `sard --version` prints it.
- Cross-compiling works the same way with a target prefix, e.g.
  `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build … -o sard.exe`.

## Configuration

Priority: CLI flag → environment variable → `.env` file (current working directory).

| Env var           | Purpose                                   | Default                     |
| ----------------- | ----------------------------------------- | --------------------------- |
| `ONYX_API_URL`    | Onyx API base URL                         | `https://cloud.onyx.app/api`|
| `ONYX_API_KEY`    | Bearer API key                            | *(required for real runs)*  |
| `ONYX_CC_PAIR_ID` | Connector-credential pair id for the docs | *(required for real runs)*  |
| `GIT_TOKEN`       | Token for private repos (injected into the clone URL) | *(optional)*   |
| `SARD_ID_BASE`    | Arbitrary document-ID base for the run (affects the ID only; PLAN §6) | *(optional)* |

`ONYX_API_KEY` and `ONYX_CC_PAIR_ID` are only required for real ingestion
runs; `--dry-run` needs neither (it sends nothing). All other flags are
documented by `sard ingest --help` and in `docs/PLAN.md` §4.

See [`.env.example`](.env.example).

## Why the name

**sardonyx** is a red-and-black banded variety of onyx (both are forms of
chalcedony) — the repo and Go module carry the full name, while the
command line tool is simply **`sard`**, the stone's own name: short, easy to
type, and unambiguous as a CLI. It builds to a single static binary: no
interpreter or virtualenv needed on the target machine.

## Repository layout

```
docs/
  PLAN.md                # full implementation plan + task breakdown (T1–T10)
  onyx-ingestion-api.md  # condensed reference for the Onyx Ingestion API
cmd/sard/
  main.go                # thin entry point: run + exit code
internal/
  cli/cli.go             # flags, pipeline orchestration, summary
  config/config.go       # flags > env > .env resolution
  models/models.go       # IngestedFile / IngestResult / OnyxPayload
  source/git.go          # URL normalization, shallow clone, git ls-files
  source/local.go        # directory walk, filters, mtime
  transform/markdown.go  # IngestedFile → Onyx payload
  onyx/client.go         # HTTP client, retries, result mapping
```

Tests live next to the code in each package as `*_test.go` (mock Onyx server
via `net/http/httptest`); git fixtures live under `internal/source/testdata/`.

## Known limitations (v1)

- **No deletion**: the Ingestion API cannot remove documents, so files deleted from the
  source keep their (stale) Onyx documents. Re-ingesting updates them, but deletion is a
  gap — see `docs/PLAN.md` §11.
- One document = one Markdown file, one section (no per-heading chunk splitting yet).
- Sequential ingestion only (no parallel uploads in v1).

## Docs & references

- [Implementation plan](docs/PLAN.md) — architecture, CLI spec, ID strategy, task list.
- [Onyx Ingestion API notes](docs/onyx-ingestion-api.md).
- Official guide: <https://docs.onyx.app/developers/guides/index_files_ingestion_api>
- Onyx core concepts: <https://docs.onyx.app/developers/core_concepts>

## License

Released under the [GNU Affero General Public License v3.0](https://www.gnu.org/licenses/agpl-3.0)
(AGPL-3.0, or a later version at your option). See [LICENSE](LICENSE).
