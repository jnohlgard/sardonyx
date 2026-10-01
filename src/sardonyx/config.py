"""Configuration resolution: CLI flags > environment variables > `.env` file.

Implements docs/PLAN.md §5.1 and §10 task T1.

Settings: api_url, api_key, cc_pair_id, branch, source override, include/exclude
globs, max_depth, max_file_size, token, dry_run, limit, log_level.
Environment variables: ONYX_API_URL, ONYX_API_KEY, ONYX_CC_PAIR_ID, GIT_TOKEN.
"""

# TODO (T1): implement the `Settings` dataclass + `resolve_settings(...)` with
# precedence tests.
