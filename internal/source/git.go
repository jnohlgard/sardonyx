// Git repository source (docs/PLAN.md §5.2, task T5).
//
// TODO (T5): URL normalization (https / git@ ssh / owner-repo shorthand;
// host → default source enum), shallow clone via os/exec into an
// os.MkdirTemp dir (deferred removal), --token injection for private repos
// (never logged), discovery via `git ls-files '*.md' '*.mdx'
// '*.markdown'`, per-file `git log -1 --format=%H%x00%ct` for the commit
// SHA and doc_updated_at, and GitHub blob URLs for section links.
package source
