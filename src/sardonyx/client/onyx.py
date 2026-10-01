"""Onyx Ingestion API HTTP client.

Implements docs/PLAN.md §7 and §10 task T6.

`OnyxClient(api_url, api_key, cc_pair_id).ingest(payload) -> IngestResult`:

- POST {api_url}/onyx-api/ingestion, Bearer auth, 30 s timeout.
- retries: 3 attempts, exponential backoff (1s/4s/16s) on 429, 5xx, timeouts;
  never retry other 4xx.
- fail fast on 401/403 with an actionable permission message.
- 200 → created (already_existed false) or updated (already_existed true);
  otherwise failed with status + body excerpt.
- the API key is never logged.
"""

# TODO (T6): implement.
