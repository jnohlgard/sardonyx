// End-to-end smoke tests for the CLI (docs/PLAN.md §10, T7). They invoke
// Run — the same entry function cmd/sard uses — with a temp-dir fixture
// and a mock Onyx server (net/http/httptest; stdlib only).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
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

	"sardonyx/internal/models"
)

// runSard invokes Run with args and returns the exit code plus whatever
// Run wrote to stdout (log output goes to the test process's stderr,
// which go test shows on failure).
func runSard(t *testing.T, args ...string) (code int, stdout []byte) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = w
	code = Run(args)
	w.Close()
	os.Stdout = old
	stdout, _ = io.ReadAll(r)
	return code, stdout
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

// fixtureDir builds a temp directory with two markdown files in stable
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
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
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

	code, out := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--dry-run", "--log-level", "error")
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
	code2, out2 := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--dry-run", "--log-level", "error")
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

	code, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--cc-pair-id", "9", "--log-level", "error")
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

	code, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--limit", "1", "--log-level", "error")
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

	code, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "error")
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

	code, _ := runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "error")
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
		code, _ := runSard(t, "ingest", dir, "--cc-pair-id", "3", "--log-level", "error")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
	t.Run("missing cc-pair id", func(t *testing.T) {
		setEnv(t, "test-key", "", "")
		dir := fixtureDir(t)
		code, _ := runSard(t, "ingest", dir, "--log-level", "error")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
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
			code, _ := runSard(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

// TestRunZeroFiles: an empty directory finds no markdown files — a
// warning, exit 0, and no payloads (dry-run) or requests (ingest).
func TestRunZeroFiles(t *testing.T) {
	setEnv(t, "test-key", "7", "")
	dir := t.TempDir()
	mock := newMockOnyx(t, func(int, models.OnyxPayload) (int, string) {
		return 200, `{}`
	})

	code, out := runSard(t, "ingest", dir, "--dry-run", "--api-url", mock.ts.URL, "--log-level", "error")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0", code)
	}
	if lines := nonEmptyLines(string(out)); len(lines) != 0 {
		t.Fatalf("dry run printed %d lines, want 0: %q", len(lines), out)
	}
	if payload, _ := mock.recorded(); len(payload) != 0 {
		t.Fatalf("sent %d payloads, want 0", len(payload))
	}

	code, _ = runSard(t, "ingest", dir, "--api-url", mock.ts.URL, "--log-level", "error")
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

	code, _ := runSard(t, "ingest", "file://"+repo, "--api-url", mock.ts.URL, "--log-level", "error")
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

	code, _ := runSard(t, "ingest", dir, "--api-url", ts.URL, "--log-level", "error")
	if code != 130 {
		t.Fatalf("exit = %d, want 130", code)
	}
}
