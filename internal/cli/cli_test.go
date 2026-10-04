// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// End-to-end smoke tests for the CLI (docs/PLAN.md §10, T7, T11b, and
// T12b). They invoke Run — the same entry function cmd/sard uses — with
// a temp-dir fixture and a mock Onyx server (net/http/httptest; stdlib
// only).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sardonyx/internal/config"
	"sardonyx/internal/models"
)

// runSard invokes Run with args and returns the exit code plus whatever
// Run wrote to stdout and to stderr. Run builds its logger on os.Stderr,
// so both os.Stdout and os.Stderr are swapped for pipes for the duration
// of the call: stdout stays strictly --dry-run JSON, and the log output
// (progress lines, summary, warnings) lands in the returned stderr
// where the T8 tests assert on it.
func runSard(t *testing.T, args ...string) (code int, stdout, stderr []byte) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdout pipe: %v", err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}
	os.Stdout = ow
	os.Stderr = ew
	code = Run(args)
	ow.Close()
	ew.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr
	stdout, _ = io.ReadAll(or)
	stderr, _ = io.ReadAll(er)
	return code, stdout, stderr
}

// lineContaining returns the first line of s that contains needle, or
// "" — the summary and progress lines are matched this way (they are
// quoted inside the TextHandler's msg= field; the needle is a
// quote-free fragment of the message).
func lineContaining(s, needle string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// assertElapsed checks that a summary line carries a well-formed
// "in <duration>" field: the tests assert presence and shape, never
// the exact wall-clock value (docs/PLAN.md §10 T8). The line is a
// slog TextHandler record: the msg value is wrapped in double
// quotes, and when the duration is the message's last field the
// closing quote follows it directly — so the msg is unwrapped
// before the field is extracted.
func assertElapsed(t *testing.T, line string) {
	t.Helper()
	if _, msg, ok := strings.Cut(line, `msg="`); ok {
		line = strings.TrimSuffix(msg, `"`)
	}
	_, rest, ok := strings.Cut(line, " in ")
	if !ok {
		t.Fatalf("no elapsed field in summary line %q", line)
	}
	token := rest
	if before, _, ok := strings.Cut(rest, " "); ok {
		token = before
	}
	d, err := time.ParseDuration(token)
	if err != nil {
		t.Fatalf("elapsed field %q is not a duration: %v", token, err)
	}
	if d <= 0 {
		t.Fatalf("elapsed field %q is not positive", token)
	}
}

// setEnv pins the configuration environment for a test: the given
// values win, everything else is cleared (config treats empty variables
// as unset), and the CWD is moved to an empty temp dir so no developer
// .env can leak in.
func setEnv(t *testing.T, apiKey, ccPairID, apiURL string) {
	t.Helper()
	t.Setenv("ONYX_API_KEY", apiKey)
	t.Setenv("ONYX_CC_PAIR_ID", ccPairID)
	t.Setenv("ONYX_API_URL", apiURL)
	t.Setenv("GIT_TOKEN", "")
	t.Setenv("SARD_ID_BASE", "")
	t.Chdir(t.TempDir())
}

// mockOnyx is a mock of the Onyx Ingestion API that records every
// request and replies via respond (n is the 1-based request number).
type mockOnyx struct {
	ts      *httptest.Server
	mu      sync.Mutex
	payload []models.OnyxPayload
	auths   []string
	respond func(n int, p models.OnyxPayload) (code int, body string)
}

func newMockOnyx(t *testing.T, respond func(n int, p models.OnyxPayload) (code int, body string)) *mockOnyx {
	t.Helper()
	m := &mockOnyx{respond: respond}
	m.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/onyx-api/ingestion" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var p models.OnyxPayload
		_ = json.Unmarshal(body, &p)
		m.mu.Lock()
		m.payload = append(m.payload, p)
		m.auths = append(m.auths, r.Header.Get("Authorization"))
		n := len(m.payload)
		m.mu.Unlock()
		code, resp := m.respond(n, p)
		w.WriteHeader(code)
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(m.ts.Close)
	return m
}

// recorded returns a copy of the requests seen so far.
func (m *mockOnyx) recorded() (payload []models.OnyxPayload, auths []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]models.OnyxPayload(nil), m.payload...), append([]string(nil), m.auths...)
}

// fixtureDir builds a temp directory with two Markdown files in stable
// RelPath order (a.md, docs/b.md).
func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"a.md":      "# Alpha\n\nalpha body\n",
		"docs/b.md": "## Beta\n\nbeta body\n", // no level-1 heading: title falls back to the basename
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

func nonEmptyLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestRunDryRunLocalDir: --dry-run prints one valid JSON payload per
// discovered file (stable RelPath order), deterministic IDs across runs,
// the right document fields, and sends nothing.
func TestRunDryRunLocalDir(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}`
	})

	code, out, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--dry-run", "--log-level", "error")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if payload, _ := mock.recorded(); len(payload) != 0 {
		t.Fatalf("dry run sent %d payloads, want 0", len(payload))
	}

	lines := nonEmptyLines(string(out))
	if len(lines) != 2 {
		t.Fatalf("printed %d lines, want 2 payloads:\n%s", len(lines), out)
	}
	// Second run: same payload bytes (deterministic IDs).
	code2, out2, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--dry-run", "--log-level", "error")
	if code2 != 0 {
		t.Fatalf("second run exit = %d, want 0", code2)
	}
	if string(out2) != string(out) {
		t.Fatalf("payloads differ across runs:\nfirst:  %s\nsecond: %s", out, out2)
	}

	var parsed []models.OnyxPayload
	want := []string{"a.md", "docs/b.md"}
	for i, line := range lines {
		var p models.OnyxPayload
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
		}
		parsed = append(parsed, p)
	}
	for i, p := range parsed {
		if len(p.Document.ID) != 64 {
			t.Errorf("payload %d: ID %q is not 64 hex chars", i, p.Document.ID)
		}
		if p.CCPairID != 7 {
			t.Errorf("payload %d: cc_pair_id = %d, want 7", i, p.CCPairID)
		}
		if p.Document.Source != models.DocumentSourceFile {
			t.Errorf("payload %d: source = %q, want %q", i, p.Document.Source, models.DocumentSourceFile)
		}
		if !p.Document.FromIngestionAPI {
			t.Errorf("payload %d: from_ingestion_api = false, want true", i)
		}
		if got := p.Document.Metadata["path"]; got != want[i] {
			t.Errorf("payload %d: metadata path = %v, want %q", i, got, want[i])
		}
		if got := p.Document.Metadata["ingested_by"]; got != "sardonyx" {
			t.Errorf("payload %d: metadata ingested_by = %v", i, got)
		}
		if _, isGit := p.Document.Metadata["commit"]; isGit {
			t.Errorf("payload %d: local document carries git metadata", i)
		}
		if len(p.Document.Sections) != 1 || p.Document.Sections[0].Text == "" {
			t.Errorf("payload %d: expected one non-empty section", i)
		}
		if p.Document.Sections[0].Link != nil {
			t.Errorf("payload %d: local document carries a link", i)
		}
	}
	if parsed[0].Document.SemanticIdentifier != filepath.Base(dir)+"/a.md" {
		t.Errorf("payload 0: semantic_identifier = %q, want %q/a.md", parsed[0].Document.SemanticIdentifier, filepath.Base(dir))
	}
	// Title from the first level-1 heading (b.md has none → basename).
	if parsed[0].Document.Title != "Alpha" {
		t.Errorf("payload 0: title = %q, want Alpha", parsed[0].Document.Title)
	}
	if parsed[1].Document.Title != "b.md" {
		t.Errorf("payload 1: title = %q, want b.md (no # heading)", parsed[1].Document.Title)
	}
}

// TestRunIngestLocalDir: a run against the mock returns 0 with the
// already_existed → updated / created mapping, in RelPath order, with
// the flag's --cc-pair-id winning over the environment and the Bearer
// auth header set.
func TestRunIngestLocalDir(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(n int, p models.OnyxPayload) (int, string) {
		switch n {
		case 1:
			return 200, `{"already_existed": true}`
		default:
			return 200, `{}`
		}
	})

	code, _, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--cc-pair-id", "9", "--log-level", "error")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	payload, auths := mock.recorded()
	if len(payload) != 2 {
		t.Fatalf("%d requests, want 2", len(payload))
	}
	for i, p := range payload {
		if p.CCPairID != 9 {
			t.Errorf("request %d: cc_pair_id = %d, want 9 (flag beats env 7)", i, p.CCPairID)
		}
		if p.Document.Source != models.DocumentSourceFile {
			t.Errorf("request %d: source = %q", i, p.Document.Source)
		}
	}
	if payload[0].Document.Metadata["path"] != "a.md" || payload[1].Document.Metadata["path"] != "docs/b.md" {
		t.Errorf("request order = %v, %v; want a.md then docs/b.md",
			payload[0].Document.Metadata["path"], payload[1].Document.Metadata["path"])
	}
	if auths[0] != "Bearer test-key" || auths[1] != "Bearer test-key" {
		t.Errorf("auth headers = %v, want Bearer test-key", auths)
	}
}

// TestRunLimit: --limit N caps the number of files ingested.
func TestRunLimit(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}`
	})

	code, _, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--limit", "1", "--log-level", "error")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	payload, _ := mock.recorded()
	if len(payload) != 1 {
		t.Fatalf("%d requests, want 1 (--limit 1)", len(payload))
	}
	if payload[0].Document.Metadata["path"] != "a.md" {
		t.Errorf("limited run sent %v, want the first file (a.md)", payload[0].Document.Metadata["path"])
	}
}

// TestRunIngestFailure: a non-retryable 400 fails the file (nil error),
// the run completes for both files, and the exit code is 1.
func TestRunIngestFailure(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 400, `{"detail": "invalid document"}`
	})

	code, _, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "error")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if payload, _ := mock.recorded(); len(payload) != 2 {
		t.Fatalf("%d requests, want 2 (per-file isolation: both attempted)", len(payload))
	}
}

// TestRunAuthFailure: a 401 fails fast — the run aborts after one
// request with exit 2 (a credentials problem, docs/PLAN.md §8).
func TestRunAuthFailure(t *testing.T) {
	setEnv(t, "bad-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 401, `{"detail": "invalid token"}`
	})

	code, _, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "error")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if payload, _ := mock.recorded(); len(payload) != 1 {
		t.Fatalf("%d requests, want 1 (aborted on the first file)", len(payload))
	}
}

// TestRunMissingCredentials: ONYX_API_KEY or ONYX_CC_PAIR_ID missing
// (flag, env, and .env all empty) is a pre-flight configuration error
// → exit 2.
func TestRunMissingCredentials(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		setEnv(t, "", "", "")
		dir := fixtureDir(t)
		code, _, _ := runSard(t, "ingest", dir, "--cc-pair-id", "3", "--log-level", "error")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
	t.Run("missing cc-pair id", func(t *testing.T) {
		setEnv(t, "test-key", "", "")
		dir := fixtureDir(t)
		code, _, _ := runSard(t, "ingest", dir, "--log-level", "error")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
}

// TestRunDryRunNoCredentials: a dry run sends nothing, so the Onyx
// credentials are not required for it — with a zero-configuration
// environment (no API key, no cc-pair id) it still prints the would-be
// payloads and exits 0 (docs/PLAN.md §4).
func TestRunDryRunNoCredentials(t *testing.T) {
	setEnv(t, "", "", "")
	dir := fixtureDir(t)
	code, out, _ := runSard(t, "ingest", dir, "--dry-run", "--log-level", "error")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := nonEmptyLines(string(out))
	if len(lines) != 2 {
		t.Fatalf("printed %d lines, want 2 payloads:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var p models.OnyxPayload
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
		}
		if p.Document.SemanticIdentifier == "" {
			t.Errorf("payload %d has an empty semantic_identifier", i)
		}
		if p.CCPairID != 0 {
			t.Errorf("payload %d has cc_pair_id = %d, want 0 (unset)", i, p.CCPairID)
		}
	}
}

// TestRunConfigurationErrors: bad flags and bad inputs are all
// configuration errors → exit 2.
func TestRunConfigurationErrors(t *testing.T) {
	dir := fixtureDir(t)
	badPath := filepath.Join(t.TempDir(), "nope")
	cases := []struct {
		name string
		args []string
	}{
		{"unknown subcommand", []string{"frobnicate", dir}},
		{"no arguments", []string{}},
		{"missing source", []string{"ingest"}},
		{"unknown --source value", []string{"ingest", dir, "--source", "bitbucket"}},
		{"negative --limit", []string{"ingest", dir, "--limit", "-1"}},
		{"negative --cc-pair-id", []string{"ingest", dir, "--cc-pair-id", "-1"}},
		{"non-int --cc-pair-id", []string{"ingest", dir, "--cc-pair-id", "abc"}},
		{"invalid --log-level", []string{"ingest", dir, "--log-level", "loud"}},
		{"unknown flag", []string{"ingest", dir, "--wat"}},
		{"bad path", []string{"ingest", badPath}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, "test-key", "7", "")
			code, _, _ := runSard(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

// TestRunZeroFiles: an empty directory finds no Markdown files — a
// warning, exit 0, and no payloads (dry-run) or requests (ingest).
func TestRunZeroFiles(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := t.TempDir()
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}`
	})

	code, out, _ := runSard(t, "ingest", dir, "--dry-run", "--api-url", mock.ts.URL, "--log-level", "error")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0", code)
	}
	if lines := nonEmptyLines(string(out)); len(lines) != 0 {
		t.Fatalf("dry run printed %d lines, want 0: %q", len(lines), out)
	}
	if payload, _ := mock.recorded(); len(payload) != 0 {
		t.Fatalf("sent %d payloads, want 0", len(payload))
	}

	code, _, _ = runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "error")
	if code != 0 {
		t.Fatalf("ingest exit = %d, want 0", code)
	}
	if payload, _ := mock.recorded(); len(payload) != 0 {
		t.Fatalf("sent %d payloads, want 0", len(payload))
	}
}

// TestRunGitStyleLocalClone: a git-style input that is not a local
// directory — a file:// URL to a fixture repo built in a temp dir —
// goes through source.Git and ingests with git provenance.
func TestRunGitStyleLocalClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if runtime.GOOS == "windows" {
		t.Skip("file:// URL form does not match Windows paths")
	}
	setEnv(t, "test-key", "7", "")
	// Isolate git from host configuration.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "sard test")
	t.Setenv("GIT_AUTHOR_EMAIL", "sard@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "sard test")
	t.Setenv("GIT_COMMITTER_EMAIL", "sard@example.com")

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("-c", "advice.defaultBranchName=false", "init")
	for name, content := range map[string]string{"a.md": "# A\n\nbody\n", "b.md": "# B\n\nbody\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-m", "initial")

	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}` // created
	})

	code, _, _ := runSard(t, "ingest", "file://"+repo, "--api-url", mock.ts.URL, "--log-level", "error")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	payload, _ := mock.recorded()
	if len(payload) != 2 {
		t.Fatalf("%d requests, want 2", len(payload))
	}
	for i, p := range payload {
		if p.Document.Source != models.DocumentSourceFile {
			t.Errorf("request %d: source = %q, want %q (file:// origin)", i, p.Document.Source, models.DocumentSourceFile)
		}
		if p.Document.Metadata["commit"] == "" {
			t.Errorf("request %d: git document missing commit metadata", i)
		}
		if p.Document.Metadata["repo"] == "" {
			t.Errorf("request %d: git document missing repo metadata", i)
		}
		if p.Document.Sections[0].Link != nil {
			t.Errorf("request %d: non-GitHub origin must not carry a link", i)
		}
	}
}

// TestRunInterrupted: SIGINT during the run cancels the in-flight
// request and the run exits 130.
func TestRunInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT semantics differ on Windows")
	}
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	// The handler holds the request open until released (or the client's
	// context cancels it); ts.Close is deferred until after release is
	// closed, or it would wait on the still-blocked handler.
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			return
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second): // safety net: never hang a test
			w.WriteHeader(200)
			fmt.Fprint(w, `{}`)
		}
	})
	ts := httptest.NewServer(h)
	defer func() {
		close(release)
		ts.Close()
	}()

	time.AfterFunc(300*time.Millisecond, func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	})

	code, _, _ := runSard(t, "ingest", dir, "--api-url", ts.URL, "--log-level", "error")
	if code != 130 {
		t.Fatalf("exit = %d, want 130", code)
	}
}

// TestRunProgressAndSummary: a successful run logs a progress line per
// file ("[i/N] <file> → <outcome>") and ends with the summary header —
// total, created, updated, skipped, failed, and the whole-run elapsed
// time (docs/PLAN.md §8). The mock is the reference for the summary
// shape (a real Onyx instance is not available, PLAN §10 T8).
func TestRunProgressAndSummary(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(n int, p models.OnyxPayload) (int, string) {
		if n == 1 {
			return 200, `{"already_existed": true}`
		}
		return 200, `{}`
	})

	code, _, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	stderrS := string(stderr)
	for _, want := range []string{"[1/2] a.md → updated", "[2/2] docs/b.md → created"} {
		if line := lineContaining(stderrS, want); line == "" {
			t.Errorf("no progress line %q; stderr:\n%s", want, stderrS)
		}
	}
	header := lineContaining(stderrS, "run complete: 2 files in")
	if header == "" {
		t.Fatalf("no summary header; stderr:\n%s", stderrS)
	}
	if !strings.Contains(header, "— created 1, updated 1, skipped 0, failed 0") {
		t.Errorf("summary counts wrong:\n%s", header)
	}
	assertElapsed(t, header)
}

// TestRunSummaryFailureList: failed files get a progress line at the
// moment of failure (warn) and are re-listed in the summary — one
// "  failed: <file> — <reason>" line each, in ingestion (RelPath)
// order (docs/PLAN.md §8).
func TestRunSummaryFailureList(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		// Plain text (no JSON quotes): keeps the assertions free of
		// TextHandler quote escaping.
		return 400, "invalid document"
	})

	code, _, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "info")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	stderrS := string(stderr)
	for _, want := range []string{
		"[1/2] a.md → failed: Onyx returned HTTP 400: invalid document",
		"[2/2] docs/b.md → failed: Onyx returned HTTP 400: invalid document",
		"  failed: a.md — Onyx returned HTTP 400: invalid document",
		"  failed: docs/b.md — Onyx returned HTTP 400: invalid document",
	} {
		if line := lineContaining(stderrS, want); line == "" {
			t.Errorf("missing line %q; stderr:\n%s", want, stderrS)
		}
	}
	header := lineContaining(stderrS, "run complete: 2 files in")
	if header == "" || !strings.Contains(header, "— created 0, updated 0, skipped 0, failed 2") {
		t.Fatalf("summary = %q; want created 0, updated 0, skipped 0, failed 2\nstderr:\n%s", header, stderrS)
	}
	assertElapsed(t, header)
	// The failure list is in ingestion order: a.md's summary line
	// precedes docs/b.md's.
	if i := strings.Index(stderrS, "  failed: a.md"); i < 0 ||
		!strings.Contains(stderrS[i:], "  failed: docs/b.md") {
		t.Errorf("failure list out of order:\n%s", stderrS)
	}
}

// skippedFixtureDir builds a temp dir with one normal file, one file
// larger than 1 KiB, and one zero-length file.
func skippedFixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"a.md":     []byte("# Alpha\n\nalpha body\n"),
		"big.md":   make([]byte, 2048), // 2 KiB of 'a's: over the 1 KiB --max-file-size
		"empty.md": {},
	}
	for name, content := range files {
		for i := range content {
			content[i] = 'a'
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// TestRunSummarySkipped: files dropped by a per-file check (oversize,
// empty) are counted as skipped in the summary; the total reflects only
// the files actually processed, and the per-file skips still warn.
func TestRunSummarySkipped(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := skippedFixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}`
	})

	code, _, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--max-file-size", "1", "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	payload, _ := mock.recorded()
	if len(payload) != 1 || payload[0].Document.Metadata["path"] != "a.md" {
		t.Fatalf("sent %d payloads, want only a.md", len(payload))
	}
	stderrS := string(stderr)
	header := lineContaining(stderrS, "run complete: 1 file in")
	if header == "" || !strings.Contains(header, "— created 1, updated 0, skipped 2, failed 0") {
		t.Fatalf("summary = %q; want created 1, updated 0, skipped 2, failed 0\nstderr:\n%s", header, stderrS)
	}
	assertElapsed(t, header)
	if n := strings.Count(stderrS, "skipping"); n < 2 {
		t.Errorf("expected at least two per-file 'skipping' warnings, got %d:\n%s", n, stderrS)
	}
}

// TestRunZeroFilesSummary: the 0-files warning carries the skipped
// count — how many discovered candidates a per-file check dropped.
func TestRunZeroFilesSummary(t *testing.T) {
	t.Run("no candidates at all", func(t *testing.T) {
		setEnv(t, "test-key", "7", "")
		dir := t.TempDir()
		mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
			return 200, `{}`
		})

		code, out, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "warn")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if len(out) != 0 {
			t.Fatalf("stdout is not empty: %q", out)
		}
		line := lineContaining(string(stderr), "no Markdown files found")
		if line == "" || !strings.Contains(line, "skipped=0") {
			t.Fatalf("warning = %q; want skipped=0", line)
		}
	})
	t.Run("all candidates skipped", func(t *testing.T) {
		setEnv(t, "test-key", "7", "")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "empty.md"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
			return 200, `{}`
		})

		code, _, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--max-file-size", "1", "--log-level", "warn")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		line := lineContaining(string(stderr), "no Markdown files found")
		if line == "" || !strings.Contains(line, "skipped=1") {
			t.Fatalf("warning = %q; want skipped=1", line)
		}
	})
}

// TestRunDryRunSummary: a dry run ends with a summary on stderr
// (payloads printed, skipped, elapsed); stdout stays strictly one JSON
// payload per file.
func TestRunDryRunSummary(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}`
	})

	code, out, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--dry-run", "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := nonEmptyLines(string(out))
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want 2:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var p models.OnyxPayload
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("stdout line %d is not JSON (stdout must carry only payloads): %v\n%s", i, err, out)
		}
	}
	if payload, _ := mock.recorded(); len(payload) != 0 {
		t.Fatalf("dry run sent %d payloads, want 0", len(payload))
	}
	header := lineContaining(string(stderr), "dry run complete: 2 files in")
	if header == "" || !strings.Contains(header, "— printed 2 payloads, skipped 0") {
		t.Fatalf("summary = %q; want printed 2 payloads, skipped 0\nstderr:\n%s", header, stderr)
	}
	assertElapsed(t, header)
}

// TestRunLimitSummary: --limit caps the number of files ingested — and
// the number of payloads printed in a dry run — and the summary's total
// reflects the capped set; the skipped count comes from discovery and
// is unaffected by the limit (T7 decision, PLAN §4).
func TestRunLimitSummary(t *testing.T) {
	t.Run("ingest", func(t *testing.T) {
		setEnv(t, "test-key", "7", "")
		dir := fixtureDir(t)
		mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
			return 200, `{}`
		})

		code, _, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--limit", "1", "--log-level", "info")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if payload, _ := mock.recorded(); len(payload) != 1 {
			t.Fatalf("sent %d payloads, want 1", len(payload))
		}
		stderrS := string(stderr)
		if line := lineContaining(stderrS, "[1/1] a.md → created"); line == "" {
			t.Fatalf("no limited progress line; stderr:\n%s", stderrS)
		}
		header := lineContaining(stderrS, "run complete: 1 file in")
		if header == "" || !strings.Contains(header, "— created 1, updated 0, skipped 0, failed 0") {
			t.Fatalf("summary = %q; want total 1, created 1\nstderr:\n%s", header, stderrS)
		}
		assertElapsed(t, header)
	})
	t.Run("dry run", func(t *testing.T) {
		setEnv(t, "test-key", "7", "")
		dir := fixtureDir(t)
		mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
			return 200, `{}`
		})

		code, out, stderr := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--dry-run", "--limit", "1", "--log-level", "info")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if lines := nonEmptyLines(string(out)); len(lines) != 1 {
			t.Fatalf("stdout has %d lines, want 1:\n%s", len(lines), out)
		}
		header := lineContaining(string(stderr), "dry run complete: 1 file in")
		if header == "" || !strings.Contains(header, "— printed 1 payload, skipped 0") {
			t.Fatalf("summary = %q; want printed 1 payload, skipped 0\nstderr:\n%s", header, stderr)
		}
		assertElapsed(t, header)
	})
}

// TestRunInterruptedSummary: SIGINT ends the run with a warn-level
// summary — the interrupted (in-flight) file appears in the failure
// list — and exit 130.
func TestRunInterruptedSummary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT semantics differ on Windows")
	}
	setEnv(t, "test-key", "7", "")
	dir := fixtureDir(t)
	// Same blocking handler as TestRunInterrupted.
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			return
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			w.WriteHeader(200)
			fmt.Fprint(w, `{}`)
		}
	})
	ts := httptest.NewServer(h)
	defer func() {
		close(release)
		ts.Close()
	}()

	time.AfterFunc(300*time.Millisecond, func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	})

	code, _, stderr := runSard(t, "ingest", dir, "--api-url", ts.URL, "--log-level", "info")
	if code != 130 {
		t.Fatalf("exit = %d, want 130", code)
	}
	stderrS := string(stderr)
	header := lineContaining(stderrS, "run interrupted: 2 files in")
	if header == "" || !strings.Contains(header, "— created 0, updated 0, skipped 0, failed 1") {
		t.Fatalf("summary = %q; want interrupted header with failed 1\nstderr:\n%s", header, stderrS)
	}
	assertElapsed(t, header)
	if line := lineContaining(stderrS, "  failed: a.md — interrupted: context canceled"); line == "" {
		t.Errorf("no failure line for the in-flight file; stderr:\n%s", stderrS)
	}
}

// TestRunVersion verifies the root command's --version flag (the cobra
// built-in activated by cli.Version): it prints "sard version
// <version>" to stdout, writes nothing to stderr, and exits 0
// (docs/PLAN.md §8). cmd/sard assigns the build-time main.version
// stamp to Version; the test assigns it directly.
func TestRunVersion(t *testing.T) {
	old := Version
	Version = "1.2.3"
	defer func() { Version = old }()

	code, out, errOut := runSard(t, "--version")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut)
	}
	if want := "sard version 1.2.3\n"; string(out) != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if len(errOut) != 0 {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
}

// TestIngestUsageTemplateEnvNames guards the static Environment
// section of ingestUsageTemplate against drift: the section's env var
// names are hand-written, so each name defined in internal/config must
// appear in it.
func TestIngestUsageTemplateEnvNames(t *testing.T) {
	for _, name := range []string{
		config.EnvAPIURL,
		config.EnvAPIKey,
		config.EnvCCPairID,
		config.EnvGitToken,
		config.EnvIDBase,
	} {
		if !strings.Contains(ingestUsageTemplate, name) {
			t.Errorf("Environment section of ingestUsageTemplate is missing %s", name)
		}
	}
}

// mockCheck is a mock of the three endpoints sard check probes
// (GET /health, GET/POST /onyx-api/ingestion, GET /manage/admin/
// cc-pair/{id}). It records every request — method, path, body, and
// auth — so the tests can assert the probe sequence and that no
// document-shaped POST ever reaches the server.
type mockCheck struct {
	ts       *httptest.Server
	mu       sync.Mutex
	requests []checkRequest
	respond  func(method, path string, n int) (code int, body string)
}

// checkRequest is one request as seen by mockCheck.
type checkRequest struct {
	Method string
	Path   string
	Body   []byte
	Auth   string
}

func newMockCheck(t *testing.T, respond func(method, path string, n int) (int, string)) *mockCheck {
	t.Helper()
	m := &mockCheck{respond: respond}
	m.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		m.requests = append(m.requests, checkRequest{r.Method, r.URL.Path, body, r.Header.Get("Authorization")})
		n := 0
		for _, req := range m.requests {
			if req.Method == r.Method && req.Path == r.URL.Path {
				n++
			}
		}
		m.mu.Unlock()

		code, resp := m.respond(r.Method, r.URL.Path, n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(m.ts.Close)
	return m
}

func (m *mockCheck) all() []checkRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]checkRequest(nil), m.requests...)
}

func (m *mockCheck) count(method, path string) int {
	n := 0
	for _, r := range m.all() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// anyDocumentPost reports whether any POST carried a document-shaped
// body (an id or sections) — the hard invariant of sard check: no
// document is ever created, updated, or deleted. The 2f fallback body
// is exactly {} and has none of those fields.
func (m *mockCheck) anyDocumentPost() bool {
	for _, r := range m.all() {
		if r.Method != http.MethodPost {
			continue
		}
		var p models.OnyxPayload
		if json.Unmarshal(r.Body, &p) == nil &&
			(p.Document.ID != "" || len(p.Document.Sections) > 0 || p.Document.SemanticIdentifier != "") {
			return true
		}
	}
	return false
}

// checkHappyServer serves all three check endpoints healthily for
// cc-pair id 7.
func checkHappyServer(t *testing.T) *mockCheck {
	t.Helper()
	return newMockCheck(t, func(method, path string, n int) (int, string) {
		switch {
		case method == http.MethodGet && path == "/health":
			return 200, `{"success":true,"message":"ok","data":null}`
		case method == http.MethodGet && path == "/onyx-api/ingestion":
			return 200, `[{"document_id":"d1","semantic_id":"a.md","link":null},{"document_id":"d2","semantic_id":"b.md","link":null}]`
		case method == http.MethodGet && path == "/manage/admin/cc-pair/7":
			return 200, `{"success":true,"data":{"name":"My Docs","status":"ACTIVE","num_docs_indexed":12}}`
		}
		return 404, `{"detail":"not found"}`
	})
}

// TestRunCheckHappy: a healthy environment exits 0 — the report lines
// (one per probe, then the final verdict line) go to stderr, stdout
// stays empty, exactly one request is made per probe (GET only; the
// Bearer header is on probes 2 and 3, not on health), and the server
// never received a document-shaped POST (docs/PLAN.md §13.4, §13.5).
func TestRunCheckHappy(t *testing.T) {
	setEnv(t, "test-check-key", "7", "")
	mock := checkHappyServer(t)

	code, out, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if len(out) != 0 {
		t.Fatalf("stdout is not empty (check prints no JSON): %q", out)
	}
	if got := mock.count(http.MethodGet, "/health"); got != 1 {
		t.Errorf("GET /health requests = %d, want 1", got)
	}
	if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
		t.Errorf("GET /onyx-api/ingestion requests = %d, want 1", got)
	}
	if got := mock.count(http.MethodGet, "/manage/admin/cc-pair/7"); got != 1 {
		t.Errorf("GET cc-pair requests = %d, want 1", got)
	}
	if got := mock.count(http.MethodPost, "/onyx-api/ingestion"); got != 0 {
		t.Errorf("POST /onyx-api/ingestion requests = %d, want 0 (a modern deployment)", got)
	}
	if mock.anyDocumentPost() {
		t.Fatal("a document-shaped POST reached the server")
	}
	for _, r := range mock.all() {
		if r.Path == "/health" && r.Auth != "" {
			t.Errorf("health probe carries Authorization %q, want none", r.Auth)
		}
		if r.Path != "/health" && r.Auth != "Bearer test-check-key" {
			t.Errorf("%s %s Authorization = %q, want Bearer test-check-key", r.Method, r.Path, r.Auth)
		}
	}

	stderrS := string(stderr)
	for _, want := range []string{
		"health ok",
		"API key accepted — 2 documents visible via the ingestion API",
		"cc-pair 7: My Docs — ACTIVE, 12 documents indexed",
		"OK: all checks passed in",
	} {
		if line := lineContaining(stderrS, want); line == "" {
			t.Errorf("no report line %q; stderr:\n%s", want, stderrS)
		}
	}
	verdict := lineContaining(stderrS, "OK: all checks passed in")
	assertElapsed(t, verdict)
}

// TestRunCheckWarnings: a degraded-but-passing environment (docs/PLAN.md
// §13.4, §13.5): health inconclusive (404 — the key probe doubles as
// the reachability test), the key accepted only via the 2f fallback
// (GET 404 → POST 400, the body rejected), and the cc-pair probe unable
// to confirm the id (a 404 in the fallback flow is not an error) →
// exit 0 with a warning per degraded probe, and the final verdict
// names the warning count: "OK: all required checks passed in …
// (3 warnings above)".
func TestRunCheckWarnings(t *testing.T) {
	setEnv(t, "test-check-key", "7", "")
	mock := newMockCheck(t, func(method, path string, n int) (int, string) {
		switch {
		case method == http.MethodGet && path == "/health":
			return 404, `{"detail":"not found"}`
		case method == http.MethodGet && path == "/onyx-api/ingestion":
			return 404, `{"detail":"not found"}`
		case method == http.MethodPost && path == "/onyx-api/ingestion":
			return 400, `{"detail":"document is required"}`
		case method == http.MethodGet && path == "/manage/admin/cc-pair/7":
			return 404, `{"detail":"not found"}`
		}
		return 404, `{"detail":"not found"}`
	})

	code, out, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if len(out) != 0 {
		t.Fatalf("stdout is not empty (check prints no JSON): %q", out)
	}
	stderrS := string(stderr)
	for _, want := range []string{
		"health check inconclusive",
		"API key accepted via the POST fallback",
		"cc-pair 7 not validated (best-effort probe)",
		"OK: all required checks passed in",
		"(3 warnings above)",
	} {
		if line := lineContaining(stderrS, want); line == "" {
			t.Errorf("no line %q; stderr:\n%s", want, stderrS)
		}
	}
	verdict := lineContaining(stderrS, "OK: all required checks passed in")
	assertElapsed(t, verdict)
}

// TestRunCheckFailures: every exit-2 case of sard check
// (docs/PLAN.md §13.4): missing API key, missing cc-pair id, a
// negative --cc-pair-id, an invalid --log-level, a 401, a 403 (via the
// 2f disambiguation), a cc-pair 404 (the message names the configured
// id, never the key), and a URL with no Ingestion API at all.
func TestRunCheckFailures(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		setEnv(t, "", "7", "")
		mock := checkHappyServer(t)
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := len(mock.all()); got != 0 {
			t.Errorf("%d requests, want 0 (a pre-flight error probes nothing)", got)
		}
		if line := lineContaining(string(stderr), "ONYX_API_KEY is required"); line == "" {
			t.Fatalf("no actionable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("missing cc-pair id", func(t *testing.T) {
		setEnv(t, "test-check-key", "", "")
		mock := checkHappyServer(t)
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := len(mock.all()); got != 0 {
			t.Errorf("%d requests, want 0 (a pre-flight error probes nothing)", got)
		}
		if line := lineContaining(string(stderr), "ONYX_CC_PAIR_ID is required"); line == "" {
			t.Fatalf("no actionable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("negative cc-pair-id", func(t *testing.T) {
		setEnv(t, "test-check-key", "7", "")
		mock := checkHappyServer(t)
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--cc-pair-id", "-1", "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := len(mock.all()); got != 0 {
			t.Errorf("%d requests, want 0 (a bad flag value probes nothing)", got)
		}
		if line := lineContaining(string(stderr), "invalid --cc-pair-id -1"); line == "" {
			t.Fatalf("no actionable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("invalid log-level", func(t *testing.T) {
		setEnv(t, "test-check-key", "7", "")
		code, _, stderr := runSard(t, "check", "--log-level", "loud")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if line := lineContaining(string(stderr), "invalid --log-level"); line == "" {
			t.Fatalf("no actionable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("401", func(t *testing.T) {
		setEnv(t, "test-check-key", "7", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			return 401, `{"detail":"invalid token"}`
		})
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
			t.Errorf("GET ingestion requests = %d, want 1 (a 401 is not retried)", got)
		}
		if got := mock.count(http.MethodGet, "/manage/admin/cc-pair/7"); got != 0 {
			t.Errorf("cc-pair requests = %d, want 0 (no further probes)", got)
		}
		stderrS := string(stderr)
		for _, want := range []string{"ONYX_API_KEY", "manage:connectors"} {
			if !strings.Contains(stderrS, want) {
				t.Errorf("error line does not mention %q; stderr:\n%s", want, stderrS)
			}
		}
	})
	t.Run("403 via fallback", func(t *testing.T) {
		setEnv(t, "test-check-key", "7", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if path == "/onyx-api/ingestion" {
				return 403, `{"detail":"forbidden"}`
			}
			return 404, `{}`
		})
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		// The 2f disambiguation ran, with the byte-exact {} body.
		var post bool
		for _, r := range mock.all() {
			if r.Method == http.MethodPost && r.Path == "/onyx-api/ingestion" {
				post = true
				if string(r.Body) != "{}" {
					t.Fatalf("2f body = %q, want byte-exact {}", r.Body)
				}
			}
		}
		if !post {
			t.Fatal("the 2f POST fallback did not run")
		}
	})
	t.Run("cc-pair 404", func(t *testing.T) {
		const key = "super-secret-check-key-98765"
		setEnv(t, key, "7", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 200, `[{"document_id":"d1"}]`
			}
			if path == "/manage/admin/cc-pair/7" {
				return 404, `{"detail":"not found"}`
			}
			return 404, `{}`
		})
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		stderrS := string(stderr)
		if line := lineContaining(stderrS, "cc-pair-id 7 not found"); line == "" {
			t.Fatalf("no line naming the configured id; stderr:\n%s", stderrS)
		}
		if strings.Contains(stderrS, key) {
			t.Fatalf("the API key leaked into the output; stderr:\n%s", stderrS)
		}
	})
	t.Run("no ingestion API at this URL", func(t *testing.T) {
		setEnv(t, "test-check-key", "7", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			return 404, `{"detail":"not found"}`
		})
		code, _, stderr := runSard(t, "check", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		stderrS := string(stderr)
		for _, want := range []string{"no Onyx Ingestion API at", "ONYX_API_URL"} {
			if !strings.Contains(stderrS, want) {
				t.Errorf("error line does not mention %q; stderr:\n%s", want, stderrS)
			}
		}
	})
}

// TestRunCheckUnreachable: a dead port is a connection failure on
// probe 1 (a single un-retried attempt) → exit 1
// (docs/PLAN.md §13.4).
func TestRunCheckUnreachable(t *testing.T) {
	setEnv(t, "test-check-key", "7", "")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // port now refuses connections

	code, _, stderr := runSard(t, "check", "--api-url", "http://"+addr, "--log-level", "info")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if line := lineContaining(string(stderr), "unreachable"); line == "" {
		t.Fatalf("no unreachable error line; stderr:\n%s", stderr)
	}
}

// TestRunCheckInterrupted: SIGINT mid-check (a /health request held
// open) cancels the check → exit 130 (docs/PLAN.md §13.4).
func TestRunCheckInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT semantics differ on Windows")
	}
	setEnv(t, "test-check-key", "7", "")
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			return
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second): // safety net: never hang a test
			w.WriteHeader(200)
			fmt.Fprint(w, `{"success":true}`)
		}
	}))
	defer func() {
		close(release)
		ts.Close()
	}()

	time.AfterFunc(300*time.Millisecond, func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	})

	code, _, stderr := runSard(t, "check", "--api-url", ts.URL, "--log-level", "info")
	if code != 130 {
		t.Fatalf("exit = %d, want 130; stderr:\n%s", code, stderr)
	}
}

// TestRunCheckHelp: sard check --help exits 0 with exactly four flags
// (api-url, api-key, cc-pair-id, log-level — no source/git/limit/
// dry-run/id-base flags) and an Environment section naming exactly the
// three Onyx variables (docs/PLAN.md §13.1).
func TestRunCheckHelp(t *testing.T) {
	setEnv(t, "", "", "")
	code, out, errOut := runSard(t, "check", "--help")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut)
	}
	if len(errOut) != 0 {
		t.Fatalf("stderr is not empty: %q", errOut)
	}
	s := string(out)

	// The Flags section (between "Flags:" and the next section or end;
	// check's help ends with Flags, so the after-part is the section).
	_, flagsSection, ok := strings.Cut(s, "\nFlags:\n")
	if !ok {
		t.Fatalf("no Flags section in the help output:\n%s", s)
	}
	var flagLines []string
	for line := range strings.SplitSeq(flagsSection, "\n") {
		if strings.HasPrefix(line, "      --") { // long flags only ("-h, --help" is the cobra built-in)
			flagLines = append(flagLines, line)
		}
	}
	if len(flagLines) != 4 {
		t.Fatalf("the Flags section has %d long flags, want 4:\n%s", len(flagLines), flagsSection)
	}
	for _, want := range []string{"--api-url", "--api-key", "--cc-pair-id", "--log-level"} {
		if !strings.Contains(flagsSection, want) {
			t.Errorf("the Flags section is missing %s:\n%s", want, flagsSection)
		}
	}
	for _, absent := range []string{"--source", "--branch", "--token", "--include", "--exclude", "--max-depth", "--max-file-size", "--limit", "--dry-run", "--id-base"} {
		if strings.Contains(s, absent) {
			t.Errorf("the help output mentions %s (check has no source/git/limit/dry-run/id-base flags)", absent)
		}
	}

	// The Environment section names exactly the three Onyx variables.
	// It sits between "Environment:" and the next section, so cut the
	// after-part at the first blank line.
	_, envSection, ok := strings.Cut(s, "\nEnvironment:\n")
	if !ok {
		t.Fatalf("no Environment section in the help output:\n%s", s)
	}
	if before, _, found := strings.Cut(envSection, "\n\n"); found {
		envSection = before
	}
	var envLines []string
	for line := range strings.SplitSeq(envSection, "\n") {
		if strings.TrimSpace(line) != "" {
			envLines = append(envLines, line)
		}
	}
	if len(envLines) != 3 {
		t.Fatalf("the Environment section has %d lines, want 3:\n%s", len(envLines), envSection)
	}
	for _, want := range []string{config.EnvAPIURL, config.EnvAPIKey, config.EnvCCPairID} {
		if !strings.Contains(envSection, want) {
			t.Errorf("the Environment section is missing %s:\n%s", want, envSection)
		}
	}
	for _, absent := range []string{config.EnvGitToken, config.EnvIDBase} {
		if strings.Contains(envSection, absent) {
			t.Errorf("the Environment section must not name %s (check has no git or ID-base flags):\n%s", absent, envSection)
		}
	}
}

// TestCheckUsageTemplateEnvNames guards the static Environment section
// of checkUsageTemplate against drift, mirroring
// TestIngestUsageTemplateEnvNames: it lists exactly the three Onyx
// variables — each name defined in internal/config that check has a
// flag for must appear, and the git/ID-base names must not.
func TestCheckUsageTemplateEnvNames(t *testing.T) {
	for _, name := range []string{
		config.EnvAPIURL,
		config.EnvAPIKey,
		config.EnvCCPairID,
	} {
		if !strings.Contains(checkUsageTemplate, name) {
			t.Errorf("Environment section of checkUsageTemplate is missing %s", name)
		}
	}
	for _, name := range []string{
		config.EnvGitToken,
		config.EnvIDBase,
	} {
		if strings.Contains(checkUsageTemplate, name) {
			t.Errorf("checkUsageTemplate must not name %s (check has no git or ID-base flags)", name)
		}
	}
}

// lsDocsServer serves the two-document list for the GET endpoint
// (everything else 404): the first document has no link, the second
// does — in that server order.
func lsDocsServer(t *testing.T) *mockCheck {
	t.Helper()
	return newMockCheck(t, func(method, path string, n int) (int, string) {
		if method == http.MethodGet && path == "/onyx-api/ingestion" {
			return 200, `[{"document_id":"d1","semantic_id":"a.md","link":null},{"document_id":"d2","semantic_id":"b.md","link":"https://example.com/b.md"}]`
		}
		return 404, `{"detail":"not found"}`
	})
}

// TestRunLsHappy: a healthy environment exits 0 — stdout carries
// strictly one line per document, in server order ("<document_id>\t
// <semantic_id>" plus a tab-separated link field only when non-empty),
// the "ls: N documents in …" summary goes to stderr, and the server
// received exactly one GET with the Bearer key (docs/PLAN.md §14.3,
// §14.4). No cc-pair id is configured — ls needs only the key.
func TestRunLsHappy(t *testing.T) {
	setEnv(t, "test-ls-key", "", "")
	mock := lsDocsServer(t)

	code, out, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	want := "d1\ta.md\nd2\tb.md\thttps://example.com/b.md\n"
	if string(out) != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
		t.Errorf("GET /onyx-api/ingestion requests = %d, want 1", got)
	}
	if got := len(mock.all()); got != 1 {
		t.Errorf("total requests = %d, want 1 (read-only: one GET, nothing else)", got)
	}
	for _, r := range mock.all() {
		if r.Auth != "Bearer test-ls-key" {
			t.Errorf("%s %s Authorization = %q, want Bearer test-ls-key", r.Method, r.Path, r.Auth)
		}
	}
	stderrS := string(stderr)
	header := lineContaining(stderrS, "ls: 2 documents in")
	if header == "" {
		t.Fatalf("no summary line; stderr:\n%s", stderrS)
	}
	assertElapsed(t, header)
}

// TestRunLsEmpty: an empty list exits 0 with a warning instead of the
// summary, and an empty stdout (docs/PLAN.md §14.3).
func TestRunLsEmpty(t *testing.T) {
	setEnv(t, "test-ls-key", "", "")
	mock := newMockCheck(t, func(method, path string, n int) (int, string) {
		if method == http.MethodGet && path == "/onyx-api/ingestion" {
			return 200, `[]`
		}
		return 404, `{"detail":"not found"}`
	})

	code, out, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (an empty list is not an error); stderr:\n%s", code, stderr)
	}
	if len(out) != 0 {
		t.Fatalf("stdout is not empty: %q", out)
	}
	if line := lineContaining(string(stderr), "no documents visible to this API key"); line == "" {
		t.Fatalf("no empty-list warning; stderr:\n%s", stderr)
	}
	if line := lineContaining(string(stderr), "ls: 0 documents"); line != "" {
		t.Fatalf("an empty list must warn, not summarize: %q", line)
	}
}

// TestRunLsFailures: every exit-2 case of sard ls (docs/PLAN.md
// §14.4): missing API key, an invalid --log-level, a 401, a 403, and
// a 404 (the message names the endpoint and the URL, never the key).
func TestRunLsFailures(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		setEnv(t, "", "", "")
		mock := lsDocsServer(t)
		code, _, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := len(mock.all()); got != 0 {
			t.Errorf("%d requests, want 0 (a pre-flight error sends nothing)", got)
		}
		if line := lineContaining(string(stderr), "ONYX_API_KEY is required"); line == "" {
			t.Fatalf("no actionable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("invalid log-level", func(t *testing.T) {
		setEnv(t, "test-ls-key", "", "")
		code, _, stderr := runSard(t, "ls", "--log-level", "loud")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if line := lineContaining(string(stderr), "invalid --log-level"); line == "" {
			t.Fatalf("no actionable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("401", func(t *testing.T) {
		const key = "super-secret-ls-key-12345"
		setEnv(t, key, "", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 401, `{"detail":"invalid token"}`
			}
			return 404, `{"detail":"not found"}`
		})
		code, _, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
			t.Errorf("GET requests = %d, want 1 (a 401 is not retried)", got)
		}
		stderrS := string(stderr)
		for _, want := range []string{"ONYX_API_KEY", "manage:connectors"} {
			if !strings.Contains(stderrS, want) {
				t.Errorf("error line does not mention %q; stderr:\n%s", want, stderrS)
			}
		}
		if strings.Contains(stderrS, key) {
			t.Fatalf("the API key leaked into the output; stderr:\n%s", stderrS)
		}
	})
	t.Run("403", func(t *testing.T) {
		const key = "super-secret-ls-key-12345"
		setEnv(t, key, "", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 403, `{"detail":"forbidden"}`
			}
			return 404, `{"detail":"not found"}`
		})
		code, _, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
			t.Errorf("GET requests = %d, want 1 (a 403 is not retried)", got)
		}
		if strings.Contains(string(stderr), key) {
			t.Fatalf("the API key leaked into the output; stderr:\n%s", stderr)
		}
	})
	t.Run("404 endpoint unavailable", func(t *testing.T) {
		const key = "super-secret-ls-key-12345"
		setEnv(t, key, "", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			return 404, `{"detail":"not found"}`
		})
		code, _, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
		}
		if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
			t.Errorf("GET requests = %d, want 1 (a 404 is not retried)", got)
		}
		if got := mock.count(http.MethodPost, "/onyx-api/ingestion"); got != 0 {
			t.Errorf("POST requests = %d, want 0 (ls has no POST fallback)", got)
		}
		stderrS := string(stderr)
		for _, want := range []string{
			"GET /onyx-api/ingestion is not available at " + mock.ts.URL,
			"ONYX_API_URL",
		} {
			if !strings.Contains(stderrS, want) {
				t.Errorf("error line does not mention %q; stderr:\n%s", want, stderrS)
			}
		}
		if strings.Contains(stderrS, key) {
			t.Fatalf("the API key leaked into the output; stderr:\n%s", stderrS)
		}
	})
}

// TestRunLsUnreachable: a dead port (a connection failure after
// retries) and a persistent 429 both exit 1 (docs/PLAN.md §14.4). Each
// subtest runs the production backoff (1 s + 4 s between the three
// attempts — the CLI layer has no injection seam), so the test takes
// about 10 s.
func TestRunLsUnreachable(t *testing.T) {
	t.Run("dead port", func(t *testing.T) {
		setEnv(t, "test-ls-key", "", "")
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close() // port now refuses connections

		code, _, stderr := runSard(t, "ls", "--api-url", "http://"+addr, "--log-level", "info")
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		if line := lineContaining(string(stderr), "unreachable"); line == "" {
			t.Fatalf("no unreachable error line; stderr:\n%s", stderr)
		}
	})
	t.Run("persistent 429", func(t *testing.T) {
		setEnv(t, "test-ls-key", "", "")
		mock := newMockCheck(t, func(method, path string, n int) (int, string) {
			return 429, `{"detail":"rate limited"}`
		})
		code, _, stderr := runSard(t, "ls", "--api-url", mock.ts.URL, "--log-level", "info")
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		if got := mock.count(http.MethodGet, "/onyx-api/ingestion"); got != 3 {
			t.Errorf("GET requests = %d, want 3 (retries exhausted)", got)
		}
		if line := lineContaining(string(stderr), "unreachable"); line == "" {
			t.Fatalf("no unreachable error line; stderr:\n%s", stderr)
		}
	})
}

// TestRunLsInterrupted: SIGINT mid-list (a GET request held open)
// cancels the request → exit 130 (docs/PLAN.md §14.4).
func TestRunLsInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT semantics differ on Windows")
	}
	setEnv(t, "test-ls-key", "", "")
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			return
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second): // safety net: never hang a test
			w.WriteHeader(200)
			fmt.Fprint(w, `[]`)
		}
	}))
	defer func() {
		close(release)
		ts.Close()
	}()

	time.AfterFunc(300*time.Millisecond, func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	})

	code, _, stderr := runSard(t, "ls", "--api-url", ts.URL, "--log-level", "info")
	if code != 130 {
		t.Fatalf("exit = %d, want 130; stderr:\n%s", code, stderr)
	}
}

// TestRunLsHelp: sard ls --help exits 0 with exactly three flags
// (api-url, api-key, log-level — no cc-pair-id and no
// source/git/limit/dry-run/id-base flags) and an Environment section
// naming exactly the two Onyx variables (docs/PLAN.md §14.1).
func TestRunLsHelp(t *testing.T) {
	setEnv(t, "", "", "")
	code, out, errOut := runSard(t, "ls", "--help")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut)
	}
	if len(errOut) != 0 {
		t.Fatalf("stderr is not empty: %q", errOut)
	}
	s := string(out)

	// The Flags section (between "Flags:" and the next section or end;
	// ls's help ends with Flags, so the after-part is the section).
	_, flagsSection, ok := strings.Cut(s, "\nFlags:\n")
	if !ok {
		t.Fatalf("no Flags section in the help output:\n%s", s)
	}
	var flagLines []string
	for line := range strings.SplitSeq(flagsSection, "\n") {
		if strings.HasPrefix(line, "      --") { // long flags only ("-h, --help" is the cobra built-in)
			flagLines = append(flagLines, line)
		}
	}
	if len(flagLines) != 3 {
		t.Fatalf("the Flags section has %d long flags, want 3:\n%s", len(flagLines), flagsSection)
	}
	for _, want := range []string{"--api-url", "--api-key", "--log-level"} {
		if !strings.Contains(flagsSection, want) {
			t.Errorf("the Flags section is missing %s:\n%s", want, flagsSection)
		}
	}
	for _, absent := range []string{"--cc-pair-id", "--source", "--branch", "--token", "--include", "--exclude", "--max-depth", "--max-file-size", "--limit", "--dry-run", "--id-base"} {
		if strings.Contains(s, absent) {
			t.Errorf("the help output mentions %s (ls has no cc-pair or source/git/limit/dry-run/id-base flags)", absent)
		}
	}

	// The Environment section names exactly the two Onyx variables.
	// It sits between "Environment:" and the next section, so cut the
	// after-part at the first blank line.
	_, envSection, ok := strings.Cut(s, "\nEnvironment:\n")
	if !ok {
		t.Fatalf("no Environment section in the help output:\n%s", s)
	}
	if before, _, found := strings.Cut(envSection, "\n\n"); found {
		envSection = before
	}
	var envLines []string
	for line := range strings.SplitSeq(envSection, "\n") {
		if strings.TrimSpace(line) != "" {
			envLines = append(envLines, line)
		}
	}
	if len(envLines) != 2 {
		t.Fatalf("the Environment section has %d lines, want 2:\n%s", len(envLines), envSection)
	}
	for _, want := range []string{config.EnvAPIURL, config.EnvAPIKey} {
		if !strings.Contains(envSection, want) {
			t.Errorf("the Environment section is missing %s:\n%s", want, envSection)
		}
	}
	for _, absent := range []string{config.EnvCCPairID, config.EnvGitToken, config.EnvIDBase} {
		if strings.Contains(envSection, absent) {
			t.Errorf("the Environment section must not name %s (ls takes no cc-pair, git, or ID-base flags):\n%s", absent, envSection)
		}
	}
}

// TestLsUsageTemplateEnvNames guards the static Environment section
// of lsUsageTemplate against drift, mirroring
// TestCheckUsageTemplateEnvNames: it lists exactly the two Onyx
// variables the ls flags stand on — the cc-pair, git, and ID-base
// names must not.
func TestLsUsageTemplateEnvNames(t *testing.T) {
	for _, name := range []string{
		config.EnvAPIURL,
		config.EnvAPIKey,
	} {
		if !strings.Contains(lsUsageTemplate, name) {
			t.Errorf("Environment section of lsUsageTemplate is missing %s", name)
		}
	}
	for _, name := range []string{
		config.EnvCCPairID,
		config.EnvGitToken,
		config.EnvIDBase,
	} {
		if strings.Contains(lsUsageTemplate, name) {
			t.Errorf("lsUsageTemplate must not name %s (ls takes no cc-pair, git, or ID-base flags)", name)
		}
	}
}

// TestRootUsageListsSubcommands: the auto-generated top-level usage
// (printed for a missing or unknown subcommand) lists all three
// subcommands.
func TestRootUsageListsSubcommands(t *testing.T) {
	for _, args := range [][]string{{}, {"frobnicate"}} {
		name := "no arguments"
		if len(args) > 0 {
			name = "unknown subcommand"
		}
		t.Run(name, func(t *testing.T) {
			setEnv(t, "", "", "")
			code, _, stderr := runSard(t, args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
			}
			_, section, ok := strings.Cut(string(stderr), "\nAvailable Commands:\n")
			if !ok {
				t.Fatalf("no Available Commands section; stderr:\n%s", stderr)
			}
			for _, want := range []string{"check", "ingest", "ls"} {
				if !strings.Contains(section, want) {
					t.Errorf("Available Commands does not list %q:\n%s", want, section)
				}
			}
		})
	}
}
