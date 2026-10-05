# Onyx Ingestion API — Reference Notes

Condensed from the official docs (fetched 2026-09-30):

- Guide: <https://docs.onyx.app/developers/guides/index_files_ingestion_api>
- Core concepts (DocumentBase / enums): <https://docs.onyx.app/developers/core_concepts>
- API reference index: <https://docs.onyx.app/developers/api_reference>
  (source of the "Other endpoints" section below, fetched 2026-10-02)

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

## Other endpoints (fetched 2026-10-02 — used by `sard check` (PLAN.md §13) and `sard ls` (PLAN.md §14))

The API reference has grown since the section above; the endpoints below
matter to Sardonyx:

- **`GET {API_BASE_URL}/health`** — **no authentication required.** Returns
  `{"success": true, "message": "…", "data": null}` on a healthy deployment.
  Reachability probe. <https://docs.onyx.app/developers/api_reference/miscellaneous/healthcheck>
- **`GET {API_BASE_URL}/onyx-api/ingestion`** — "Get Ingestion Docs": the
  read-only sibling of the `POST` above. Same Bearer auth and the same
  `manage:connectors` / `admin` permission requirement. Returns an array of
  `{"document_id", "semantic_id", "link"}` for the documents the key can
  see. A `200` with the configured key proves the key is valid *for
  ingestion* without creating, updating, or deleting anything — the
  credential check `sard check` relies on, and it is the data source of
  the read-only `sard ls` listing (PLAN.md §14).
  <https://docs.onyx.app/developers/api_reference/ingestion/get_ingestion_docs>
- **`GET {API_BASE_URL}/manage/admin/cc-pair/{cc_pair_id}`** — "Get CC Pair
  Full Info": Bearer auth; `read:connectors` (included in `manage:connectors`
  and in Manage Groups; admin has it), a Group Manager limited to pairs in
  the groups they manage. Returns the pair's name, status,
  `num_docs_indexed`, and connector/credential snapshots — enough to verify
  that a configured `cc_pair_id` actually exists on the deployment (the
  Ingestion `POST` does **not** validate that: a bogus id is accepted, and
  the documents simply never appear on the Connectors page).
  <https://docs.onyx.app/developers/api_reference/files_connectors/get_cc_pair_full_info>
- **`DELETE {API_BASE_URL}/onyx-api/ingestion/{document_id}`** — "Delete
  Ingestion Doc": removes one document by its `document_id` (the stable id
  `sard` generates and that `sard ls` lists). Same Bearer auth and the same
  `manage:connectors` / `admin` permission as the `POST` above (a Group
  Manager may also call it, limited to the pairs in the groups they
  manage); `200` on success. **`sard` does not implement this yet** — v1 is
  upsert-only and stale documents are documented as a limitation — but a
  future deletion/prune feature is a plain call to this endpoint (PLAN.md
  §11 #11); `sard check` does not depend on it.
  <https://docs.onyx.app/developers/api_reference/ingestion/delete_ingestion_doc>

Schema drift note: in the current OpenAPI the request body's `cc_pair_id`
is `integer | null` (nullable); a document still needs a valid pair to show
up on the Connectors page, but the API itself accepts `null`.

---

## Implications for `Sardonyx`

| Concern | How we handle it |
| ------- | ---------------- |
| Stable `id` | deterministic hash per file (see PLAN.md §6) → re-runs update, not duplicate |
| `cc_pair_id` | user supplies it (flag/env); we do not create connectors ourselves |
| `source` | `github` / `gitlab` / `file` by default, `--doc-source` override |
| `doc_updated_at` | last commit timestamp (git) or file mtime (local) |
| `sections[0].link` | GitHub blob URL when a public GitHub URL was the input, else null |
| `metadata` | repo, path, commit sha, `ingested_by` |
| `chunk_count`, owners, images | omitted in v1 |
| Deletion of removed files | **not implemented in v1** → documented as a limitation. The API itself supports it — the `DELETE …/ingestion/{document_id}` entry in "Other endpoints" above; deletion/prune is a v2 item (PLAN.md §11 #11) |
