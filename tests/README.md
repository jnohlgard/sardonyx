# Tests for onyx-git-ingest

Layout mirrors `docs/PLAN.md` §12 (testing strategy):

- `test_url_normalize.py`     — git URL normalization (T5)
- `test_doc_id_stability.py`  — deterministic document IDs, upsert semantics (T3)
- `test_transform.py`         — markdown → Onyx payload mapping (T3)
- `test_local_dir.py`         — directory walking, filters, exclusions (T4)
- `test_onyx_client.py`       — HTTP behavior vs a mock server: success, 429 retry,
                                5xx exhaustion, 401 fail-fast (T6)

Fixture: a small markdown repo under `tests/fixtures/` for the git integration tests.
