"""Shared data models.

Implements docs/PLAN.md §5.4 and §10 task T2:

- `IngestedFile`: a discovered markdown file with provenance
  (kind, root label, relative path, content, doc_updated_at, commit_sha, blob_url).
- `IngestResult`: per-document outcome of the Onyx call
  (status: created | updated | skipped | failed, plus reason).
- The Onyx payload dict shape lives in `transform/markdown.py`.
"""

# TODO (T2): define the dataclasses.
