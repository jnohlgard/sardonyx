// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

package models

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// TestOnyxPayloadJSONShape pins the Ingestion API request schema
// (docs/onyx-ingestion-api.md) against future JSON tag drift: the exact
// top-level and document key sets, from_ingestion_api, the source enum
// value, and link presence (github) / omission (local).
func TestOnyxPayloadJSONShape(t *testing.T) {
	link := "https://github.com/o/r/blob/main/readme.md"
	git := OnyxPayload{
		Document: OnyxDocument{
			ID:                 "git-0123456789abcdef",
			SemanticIdentifier: "o/r/readme.md",
			Title:              "Readme",
			Sections:           []Section{{Text: "# Readme", Link: &link}},
			Source:             DocumentSourceGitHub,
			Metadata:           map[string]any{"repo": "o/r", "path": "readme.md", "commit": "abc123"},
			DocUpdatedAt:       "2026-01-02T03:04:05Z",
			FromIngestionAPI:   true,
		},
		CCPairID: 243,
	}
	raw, err := json.Marshal(git)
	if err != nil {
		t.Fatalf("json.Marshal(git): %v", err)
	}

	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("json.Unmarshal(git): %v", err)
	}

	if got, want := keysOf(top), []string{"cc_pair_id", "document"}; !reflect.DeepEqual(got, want) {
		t.Errorf("top-level keys = %v, want %v", got, want)
	}
	if got, want := top["cc_pair_id"], float64(243); got != want {
		t.Errorf("cc_pair_id = %v, want %v", got, want)
	}

	doc, ok := top["document"].(map[string]any)
	if !ok {
		t.Fatalf("document is not a JSON object: %s", raw)
	}
	wantDoc := []string{
		"doc_updated_at", "from_ingestion_api", "id", "metadata",
		"sections", "semantic_identifier", "source", "title",
	}
	if got := keysOf(doc); !reflect.DeepEqual(got, wantDoc) {
		t.Errorf("document keys = %v, want %v", got, wantDoc)
	}
	if got, want := doc["from_ingestion_api"], true; got != want {
		t.Errorf("from_ingestion_api = %v, want %v", got, want)
	}
	if got, want := doc["source"], "github"; got != want {
		t.Errorf("source = %v, want %v", got, want)
	}
	if got, want := doc["doc_updated_at"], "2026-01-02T03:04:05Z"; got != want {
		t.Errorf("doc_updated_at = %v, want %v", got, want)
	}

	sec := doc["sections"].([]any)[0].(map[string]any)
	if _, has := sec["link"]; !has {
		t.Errorf("github section should carry its link: %v", sec)
	}

	local := OnyxPayload{
		Document: OnyxDocument{
			ID:                 "local-0123456789abcdef",
			SemanticIdentifier: "mydocs/readme.md",
			Title:              "Readme",
			Sections:           []Section{{Text: "# Readme"}},
			Source:             DocumentSourceFile,
			Metadata:           map[string]any{"path": "readme.md", "ingested_by": "sardonyx"},
			DocUpdatedAt:       "2026-01-02T03:04:05Z",
			FromIngestionAPI:   true,
		},
		CCPairID: 243,
	}
	rawLocal, err := json.Marshal(local)
	if err != nil {
		t.Fatalf("json.Marshal(local): %v", err)
	}
	var topLocal map[string]any
	if err := json.Unmarshal(rawLocal, &topLocal); err != nil {
		t.Fatalf("json.Unmarshal(local): %v", err)
	}
	secLocal := topLocal["document"].(map[string]any)["sections"].([]any)[0].(map[string]any)
	if _, has := secLocal["link"]; has {
		t.Errorf("local section must omit the link key entirely: %v", secLocal)
	}
}

func TestIngestResultOK(t *testing.T) {
	tests := []struct {
		status string
		wantOK bool
	}{
		{ResultCreated, true},
		{ResultUpdated, true},
		{ResultFailed, false},
		{"", false},
	}
	for _, tc := range tests {
		r := IngestResult{Status: tc.status, Reason: "boom"}
		if got := r.OK(); got != tc.wantOK {
			t.Errorf("OK() for status %q = %v, want %v", tc.status, got, tc.wantOK)
		}
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
