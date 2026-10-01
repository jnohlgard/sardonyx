// Package models holds the record types shared across the pipeline
// (docs/PLAN.md §5.4 and §6).
//
// TODO (T2): IngestedFile (kind, root label, rel path, content,
// doc_updated_at, commit SHA, blob URL), IngestResult (created | updated |
// failed + reason), and OnyxPayload — the JSON-typed request body for the
// Ingestion API.
package models
