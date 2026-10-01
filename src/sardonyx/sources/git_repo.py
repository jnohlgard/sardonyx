"""Git repository source.

Implements docs/PLAN.md §5.2 and §10 task T5:

- normalize repo URLs (https / git@ ssh / `owner/repo` shorthand) — pure function,
  unit-tested; host detection picks the default document `source` (github/gitlab/file).
- shallow clone (`git clone --depth 1 [--branch …]`) into a temp dir, cleaned up after;
  private-repo token injection (`x-access-token`) that never appears in logs.
- discover tracked markdown files via `git ls-files '*.md' '*.mdx' '*.markdown'`
  (respects the repo's own .gitignore).
- per-file provenance: `git log -1 --format=%H%x00%ct -- <path>` → commit SHA +
  last-commit timestamp (doc_updated_at); GitHub blob URL as the section link.
"""

# TODO (T5): implement.
