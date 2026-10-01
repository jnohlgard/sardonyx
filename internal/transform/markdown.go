// Package transform maps discovered files to Onyx Ingestion API payloads
// (docs/PLAN.md §6, task T3).
//
// TODO (T3): ToOnyxPayload(f models.IngestedFile, source string,
// ccPairID int) models.OnyxPayload — deterministic document ID, semantic
// identifier, title from the first heading, sections, source enum,
// metadata, and RFC-3339 UTC timestamps. Pure function, no I/O.
package transform
