"""Local directory source.

Implements docs/PLAN.md §5.3 and §10 task T4:

- validate the path is an existing directory (else config error, exit code 2).
- if the directory is (inside) a git repo, prefer `git ls-files` (tracked files +
  commit metadata); otherwise walk with pathlib.
- default exclusions: .git, node_modules, dist, build, vendor, .venv, __pycache__,
  .idea; overridable with --include / --exclude globs and --max-depth.
- doc_updated_at = file mtime (UTC); content read as UTF-8 with errors="replace".
"""

# TODO (T4): implement.
