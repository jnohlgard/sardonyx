"""Markdown → Onyx IngestionDocument payload.

Implements docs/PLAN.md §6 and §10 task T3. Pure function, no I/O.

`to_onyx_payload(file: IngestedFile, source: str, cc_pair_id: int) -> dict` maps:

- id: stable sha256 — git: "git\\0" + normalized origin + "\\0" + relpath;
      local: "local\\0" + normalized absolute path. (Branch deliberately excluded.)
- semantic_identifier: `owner/repo/path/file.md` (git) or `<dir>/path/file.md` (local).
- title: first `# ` heading, else the file name.
- sections: [{"text": content, "link": blob_url or None}].
- source: github / gitlab / file (or explicit override).
- metadata: {repo, path, commit, ingested_by: "sardonyx"} (git);
            {path, ingested_by: "sardonyx"} (local).
- doc_updated_at: ISO-8601 UTC (commit time or mtime).
- from_ingestion_api: true.  chunk_count / owners / images: intentionally omitted.

See docs/onyx-ingestion-api.md for the full field reference.
"""

# TODO (T3): implement.
