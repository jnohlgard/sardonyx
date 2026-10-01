# Onyx Ingestion API — Reference Notes

Condensed from the official docs (fetched 2026-09-30):

- Guide: <https://docs.onyx.app/developers/guides/index_files_ingestion_api>
- Core concepts (DocumentBase / enums): <https://docs.onyx.app/developers/core_concepts>

---

## Endpoint

```
POST {API_BASE_URL}/onyx-api/ingestion
```

- Cloud: `API_BASE_URL = https://cloud.onyx.app/api`
- Self-hosted: your own Onyx domain.
- Headers:
  - `Authorization: Bearer {API_KEY}`
  - `Content-Type: application/json`
- **Required permission:** `manage:connectors` or `admin`. A Group Manager may ingest into
  connector-credential pairs of the groups they manage, but **not** into the default
  (public) pair.

### When to use the Ingestion API

- Sources without a built-in Onyx Connector.
- Supplemental data (e.g. README files alongside a GitLab connector).
- Editing documents when the Admin can't update the original source.
- Programmatic / pipeline-driven indexing.

For more control Onyx suggests building a proper **Connector** (Admin Panel or the
"Create a Connector" API guide) — that is *out of scope* for this project; the Ingestion
API is the lightweight route.

---

## Request payload

```jsonc
{
  "document": {
    // ---- core (required) ----
    "semantic_identifier": "Onyx FAQ - Title shown in UI",   // shown as the doc name
    "sections": [
      { "text": "What is Onyx?\nOnyx is...", "link": "https://docs.onyx.app/faq#what-is-onyx" },
      { "text": "How do I get started?..." }                  // link is optional
      // image sections exist too, but require a prior POST /user/file/upload
      // to get an image_file_id — not used by Sardonyx
    ],
    "metadata": { "category": "faq", "tags": ["frequently-asked", "help"] },

    // ---- optional ----
    "id": "my_unique_id_1",          // if omitted, Onyx generates one.
                                     // supplying a stable id makes re-sends an UPDATE
                                     // (response then carries already_existed: true)
    "title": "Onyx FAQ v1 - Title for Search",  // search title; defaults to semantic_identifier
    "source": "file",                // DocumentSource enum, see below
    "doc_updated_at": "2025-09-19T08:20:00Z",
    "chunk_count": 15,               // we skip this and let Onyx compute it
    "primary_owners": [ { "display_name": "...", "email": "..." } ],
    "secondary_owners": [ ... ],
    "from_ingestion_api": true
  },
  "cc_pair_id": 243
}
```

Notes:

- **`cc_pair_id` is required for the document to appear on the Connectors page.**
  It's visible in the Admin Panel URL: `https://cloud.onyx.app/admin/connector/308` →
  `cc_pair_id = 243`.
- **`metadata`** values must be `string` or `list[string]`; they are stored as tags on the
  document.
- **`source`** is the `DocumentSource` enum. Relevant values for us:
  `file`, `github`, `gitlab`, `web`, and `ingestion_api` (a special value used when no
  source type is specified).
- **`doc_updated_at`** is a UTC datetime — Onyx uses it for freshness.
- **`id`**: *we* generate a stable one; it is the key to idempotent upserts.

---

## Response

- `200` → document **accepted** (indexing happens asynchronously in the background).
  The JSON body includes:
  - `already_existed: true`  → this `id` was seen before ⇒ the document was **updated**
  - `already_existed: false` ⇒ a **new** document was created
- Non-200 → read the body for the error message (e.g. bad API key, insufficient
  permission, invalid payload).

### Minimal working example (from the docs)

```python
import requests

API_BASE_URL = "https://cloud.onyx.app/api"  # or your own domain
API_KEY = "YOUR_KEY_HERE"

headers = {
    "Authorization": f"Bearer {API_KEY}",
    "Content-Type": "application/json",
}

payload = {
    "document": {
        "id": "ingestion_document_1",
        "semantic_identifier": "Onyx Ingestion Example",
        "sections": [{ "text": "This is the contents of the document...",
                       "link": "https://docs.onyx.app/introduction#what-is-onyx" }],
        "source": "file",
        "metadata": { "tag": "informational", "topics": ["onyx", "api"] },
        "doc_updated_at": "2025-09-19T08:20:00Z",
    },
    "cc_pair_id": 243,
}

r = requests.post(f"{API_BASE_URL}/onyx-api/ingestion", headers=headers, json=payload)
print(r.status_code, r.text)
```

---

## Implications for `Sardonyx`

| Concern | How we handle it |
| ------- | ---------------- |
| Stable `id` | deterministic hash per file (see PLAN.md §6) → re-runs update, not duplicate |
| `cc_pair_id` | user supplies it (flag/env); we do not create connectors ourselves |
| `source` | `github` / `gitlab` / `file` by default, `--source` override |
| `doc_updated_at` | last commit timestamp (git) or file mtime (local) |
| `sections[0].link` | GitHub blob URL when a public GitHub URL was the input, else null |
| `metadata` | repo, path, commit sha, `ingested_by` |
| `chunk_count`, owners, images | omitted in v1 |
| Deletion of removed files | **not supported** by this API → documented as a limitation |
