package source

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sardonyx/internal/config"
	"sardonyx/internal/models"
)

// mustNormalize normalizes in, failing the test on error.
func mustNormalize(t *testing.T, in string) NormalizedURL {
	t.Helper()
	n, err := NormalizeURL(in)
	if err != nil {
		t.Fatalf("NormalizeURL(%q): %v", in, err)
	}
	return n
}

// TestNormalizeURL covers the pure normalization contract
// (docs/PLAN.md §5.2): the three input forms, the .git suffix,
// embedded credentials, host → source-enum mapping, and the ID base.
func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want NormalizedURL
	}{
		{
			name: "https unchanged",
			in:   "https://github.com/owner/repo",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "https keeps .git in URL, ID base strips it",
			in:   "https://github.com/owner/repo.git",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo.git",
				Host:   "github.com",
				Path:   "owner/repo.git",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "https host lowercased",
			in:   "https://GITHUB.COM/owner/repo",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "embedded credentials split into Auth, out of URL and ID base",
			in:   "https://bob:s3kr3t@github.com/owner/repo",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
				Auth:   "bob:s3kr3t",
			},
		},
		{
			name: "ssh scp form converted to https",
			in:   "git@github.com:owner/repo",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "ssh scp form .git stripped",
			in:   "git@github.com:owner/repo.git",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "ssh scp form, gitlab subdomain → gitlab",
			in:   "gitlab@gitlab.example.com:group/project",
			want: NormalizedURL{
				URL:    "https://gitlab.example.com/group/project",
				Host:   "gitlab.example.com",
				Path:   "group/project",
				Repo:   "group/project",
				Source: models.DocumentSourceGitLab,
				IDBase: "https://gitlab.example.com/group/project",
			},
		},
		{
			name: "ssh:// URL converted, bare user dropped",
			in:   "ssh://git@github.com/owner/repo.git",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "ssh:// URL with user:password kept for pass-through",
			in:   "ssh://git:tok@gitlab.com/group/project",
			want: NormalizedURL{
				URL:    "https://gitlab.com/group/project",
				Host:   "gitlab.com",
				Path:   "group/project",
				Repo:   "group/project",
				Source: models.DocumentSourceGitLab,
				IDBase: "https://gitlab.com/group/project",
				Auth:   "git:tok",
			},
		},
		{
			name: "owner/repo shorthand → github.com",
			in:   "owner/repo",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo",
				Host:   "github.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "shorthand with .git keeps it in URL, strips it in ID base",
			in:   "owner/repo.git",
			want: NormalizedURL{
				URL:    "https://github.com/owner/repo.git",
				Host:   "github.com",
				Path:   "owner/repo.git",
				Repo:   "owner/repo",
				Source: models.DocumentSourceGitHub,
				IDBase: "https://github.com/owner/repo",
			},
		},
		{
			name: "gitlab.com → gitlab, URL unchanged",
			in:   "https://gitlab.com/group/project",
			want: NormalizedURL{
				URL:    "https://gitlab.com/group/project",
				Host:   "gitlab.com",
				Path:   "group/project",
				Repo:   "group/project",
				Source: models.DocumentSourceGitLab,
				IDBase: "https://gitlab.com/group/project",
			},
		},
		{
			name: "non-GitHub/GitLab host → file",
			in:   "https://bitbucket.org/owner/repo",
			want: NormalizedURL{
				URL:    "https://bitbucket.org/owner/repo",
				Host:   "bitbucket.org",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceFile,
				IDBase: "https://bitbucket.org/owner/repo",
			},
		},
		{
			name: "host with port kept, → file",
			in:   "https://git.example.com:8443/owner/repo",
			want: NormalizedURL{
				URL:    "https://git.example.com:8443/owner/repo",
				Host:   "git.example.com:8443",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceFile,
				IDBase: "https://git.example.com:8443/owner/repo",
			},
		},
		{
			name: "http scheme accepted, → file",
			in:   "http://git.example.com/owner/repo",
			want: NormalizedURL{
				URL:    "http://git.example.com/owner/repo",
				Host:   "git.example.com",
				Path:   "owner/repo",
				Repo:   "owner/repo",
				Source: models.DocumentSourceFile,
				IDBase: "http://git.example.com/owner/repo",
			},
		},
		{
			name: "file:// local origin",
			in:   "file:///abs/repo",
			want: NormalizedURL{
				URL:    "file:///abs/repo",
				Host:   "",
				Path:   "/abs/repo",
				Repo:   "repo",
				Source: models.DocumentSourceFile,
				IDBase: "file:///abs/repo",
				Local:  true,
			},
		},
		{
			name: "file:// .git stripped from ID base",
			in:   "file:///abs/repo.git",
			want: NormalizedURL{
				URL:    "file:///abs/repo.git",
				Host:   "",
				Path:   "/abs/repo.git",
				Repo:   "repo",
				Source: models.DocumentSourceFile,
				IDBase: "file:///abs/repo",
				Local:  true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeURL(tc.in)
			if err != nil {
				t.Fatalf("NormalizeURL(%q) = error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeURL(%q):\n got  %+v\n want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeURLErrors: anything outside the recognized forms is an
// error (Git wraps it into config.ErrConfiguration, exit code 2).
func TestNormalizeURLErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"not a url",
		"owner",
		"owner/",
		"/owner",
		"a/b/c",
		"ftp://x/y/z",
		"https://",
		"https://github.com/",
		"git@host",
		"git@:owner/repo",
	} {
		if got, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q) = %+v, want an error", in, got)
		}
	}
}

// TestCloneURL: the clone URL is the clean origin by default, the
// x-access-token-injected form when a token is supplied, the embedded
// credentials passed through (token ignored) when the input carries
// them, and a local path never gets a token.
func TestCloneURL(t *testing.T) {
	plain := mustNormalize(t, "https://github.com/owner/repo")
	if got := plain.CloneURL(""); got != "https://github.com/owner/repo" {
		t.Errorf("plain.CloneURL(\"\") = %q", got)
	}
	if got := plain.CloneURL("tok-123"); got != "https://x-access-token:tok-123@github.com/owner/repo" {
		t.Errorf("plain.CloneURL(token) = %q", got)
	}

	authed := mustNormalize(t, "https://bob:s3kr3t@github.com/owner/repo.git")
	want := "https://bob:s3kr3t@github.com/owner/repo.git"
	if got := authed.CloneURL(""); got != want {
		t.Errorf("authed.CloneURL(\"\") = %q, want %q", got, want)
	}
	// A supplied token never replaces embedded credentials.
	if got := authed.CloneURL("tok-123"); got != want {
		t.Errorf("authed.CloneURL(token) = %q, want the pass-through %q", got, want)
	}

	local := mustNormalize(t, "file:///abs/repo")
	if got := local.CloneURL("tok-123"); got != "file:///abs/repo" {
		t.Errorf("local.CloneURL(token) = %q, want the path unchanged", got)
	}
}

// TestSectionLink: the GitHub blob URL is built only for github.com
// origins; every other origin yields "" (the transform omits the link).
func TestSectionLink(t *testing.T) {
	gh := mustNormalize(t, "https://github.com/owner/repo")
	if got := gh.sectionLink("main", "docs/api.md"); got != "https://github.com/owner/repo/blob/main/docs/api.md" {
		t.Errorf("github sectionLink = %q", got)
	}
	// A github.com origin with embedded credentials (a private repo)
	// still links: the blob URL describes the origin, not a
	// public-state assertion.
	ghAuthed := mustNormalize(t, "https://bob:s3kr3t@github.com/owner/repo")
	if got := ghAuthed.sectionLink("main", "a.md"); got != "https://github.com/owner/repo/blob/main/a.md" {
		t.Errorf("github (authed) sectionLink = %q", got)
	}
	if got := mustNormalize(t, "https://gitlab.com/group/project").sectionLink("main", "a.md"); got != "" {
		t.Errorf("gitlab sectionLink = %q, want empty", got)
	}
	if got := mustNormalize(t, "file:///abs/repo").sectionLink("main", "a.md"); got != "" {
		t.Errorf("file sectionLink = %q, want empty", got)
	}
}

// gitFixtureRepo builds a small fixture repository in a fresh temp
// directory: three committed markdown files (README.md, docs/intro.md,
// guide.mdx) in two commits with fixed committer dates, a gitignored
// scratch.md, and a non-markdown notes.txt. It returns the repo root,
// and the SHA + committer time of each commit.
//
// The fixture is built in a temp directory — never committed under
// testdata/, which would be a repository inside this repository
// (docs/PLAN.md §10 T5, §12).
func gitFixtureRepo(t *testing.T) (string, string, time.Time, string, time.Time) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH (a runtime dependency, docs/PLAN.md §12)")
	}

	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main", ".")

	gitLog1 := func(t *testing.T, dir string) (string, string) {
		t.Helper()
		cmd := exec.Command("git", "-c", "core.quotePath=off", "-C", dir,
			"log", "-1", "--format=%H%x00%ct")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git log: %v", err)
		}
		parts := strings.SplitN(strings.TrimSpace(string(out)), "\x00", 2)
		if len(parts) != 2 {
			t.Fatalf("unexpected git log output %q", out)
		}
		return parts[0], parts[1]
	}
	commitAt := func(t *testing.T, when, msg string) (string, time.Time) {
		t.Helper()
		cmd := exec.Command("git", "-c", "user.name=test", "-c", "user.email=test@example.com",
			"commit", "-q", "-m", msg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+when, "GIT_AUTHOR_DATE="+when)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git commit %q: %v\n%s", msg, err, out)
		}
		sha, ct := gitLog1(t, root)
		sec, err := strconv.Atoi(ct)
		if err != nil {
			t.Fatalf("parsing committer time %q: %v", ct, err)
		}
		return sha, time.Unix(int64(sec), 0).UTC()
	}

	writeTree(t, root, map[string]string{
		"README.md":     "# sard git fixture\n\nRoot readme.\n",
		"docs/intro.md": "# Intro\n\nDocs level one.\n",
		"guide.mdx":     "# Mdx guide\n\nAn MDX file.\n",
		".gitignore":    "scratch.md\n",
		"scratch.md":    "# Ignored\n",
		"notes.txt":     "not markdown\n",
	})
	runGit(t, root, "add", "README.md", "docs/intro.md", "guide.mdx", ".gitignore")
	shaA, timeA := commitAt(t, "2024-01-02T03:04:05+00:00", "initial docs")
	// A second commit touches only README.md.
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# sard git fixture\n\nRoot readme, revised.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, root, "add", "README.md")
	shaB, timeB := commitAt(t, "2024-06-07T08:09:10+00:00", "revise readme")

	return root, shaA, timeA, shaB, timeB
}

// TestGit clones the fixture repository from a local path and checks
// the full §5.2 contract: tracked-only discovery (the repository's
// .gitignore respected for free), the record shape, commit
// provenance, the default ID base, the resolved branch, and the
// filters.
//
// Provenance in a depth-1 clone is the resolved HEAD commit for every
// file — the clone's visible history is a single commit (see the file
// comment). The two-commit fixture asserts that from both sides:
// cloning main attributes every file to the later commit, while
// cloning the tag on the earlier commit attributes every file to that
// one. Skipped when git is not on PATH (docs/PLAN.md §12).
func TestGit(t *testing.T) {
	root, shaA, timeA, shaB, timeB := gitFixtureRepo(t)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	res, err := Git(root, GitOptions{Branch: "main"}, log)
	if err != nil {
		t.Fatalf("Git: %v", err)
	}

	// Tracked-only discovery: scratch.md (gitignored) and notes.txt
	// (non-markdown) are absent; the .md/.mdx set is present, sorted
	// by RelPath.
	files := wantPaths(t, res.Files, "README.md", "docs/intro.md", "guide.mdx")

	abs, _ := filepath.Abs(root)
	wantBase := (&url.URL{Scheme: "file", Path: abs}).String()
	if res.IDBase != wantBase {
		t.Errorf("IDBase = %q, want %q", res.IDBase, wantBase)
	}
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want main", res.Branch)
	}
	if !strings.Contains(buf.String(), "cloned repository") {
		t.Errorf("expected the clone info line in the log output:\n%s", buf.String())
	}

	byPath := make(map[string]models.IngestedFile, len(files))
	for _, f := range files {
		byPath[f.RelPath] = f
	}
	for _, f := range files {
		if f.Kind != models.KindGit {
			t.Errorf("%s: Kind = %q, want git", f.RelPath, f.Kind)
		}
		if f.RootLabel != filepath.Base(root) {
			t.Errorf("%s: RootLabel = %q, want %q", f.RelPath, f.RootLabel, filepath.Base(root))
		}
		if f.BlobURL != "" {
			t.Errorf("%s: BlobURL = %q, want empty (not a github.com origin)", f.RelPath, f.BlobURL)
		}
		if len(f.CommitSHA) != 40 {
			t.Errorf("%s: CommitSHA = %q, want a 40-char SHA", f.RelPath, f.CommitSHA)
		}
		if f.DocUpdatedAt.Location() != time.UTC {
			t.Errorf("%s: DocUpdatedAt location = %v, want UTC", f.RelPath, f.DocUpdatedAt.Location())
		}
	}
	// Shallow-clone attribution: the depth-1 clone contains only the
	// resolved HEAD commit (the later one), so every file carries its
	// SHA and committer time — the file's true last-modified commit is
	// older, but that is the documented, safe upper bound.
	for _, f := range files {
		if f.CommitSHA != shaB || !f.DocUpdatedAt.Equal(timeB) {
			t.Errorf("%s: CommitSHA=%q DocUpdatedAt=%v, want the HEAD commit %q %v",
				f.RelPath, f.CommitSHA, f.DocUpdatedAt, shaB, timeB)
		}
	}
	if got := byPath["docs/intro.md"].Content; got != "# Intro\n\nDocs level one.\n" {
		t.Errorf("docs/intro.md: Content = %q", got)
	}

	t.Run("file:// input normalizes to the same ID base", func(t *testing.T) {
		res, err := Git("file://"+abs, GitOptions{}, discard())
		if err != nil {
			t.Fatalf("Git(file://): %v", err)
		}
		if res.IDBase != wantBase {
			t.Errorf("IDBase = %q, want %q (same as the bare-path form)", res.IDBase, wantBase)
		}
		if res.Branch != "main" {
			t.Errorf("Branch = %q, want main", res.Branch)
		}
		wantPaths(t, res.Files, "README.md", "docs/intro.md", "guide.mdx")
	})

	t.Run("include filter", func(t *testing.T) {
		res, err := Git(root, GitOptions{Include: []string{"docs/**"}}, discard())
		if err != nil {
			t.Fatalf("Git(Include): %v", err)
		}
		wantPaths(t, res.Files, "docs/intro.md")
	})

	t.Run("exclude filter", func(t *testing.T) {
		res, err := Git(root, GitOptions{Exclude: []string{"docs/**", "*.mdx"}}, discard())
		if err != nil {
			t.Fatalf("Git(Exclude): %v", err)
		}
		wantPaths(t, res.Files, "README.md")
	})

	t.Run("max depth", func(t *testing.T) {
		res, err := Git(root, GitOptions{MaxDepth: 1}, discard())
		if err != nil {
			t.Fatalf("Git(MaxDepth): %v", err)
		}
		wantPaths(t, res.Files, "README.md", "guide.mdx")
	})

	t.Run("cloning a tag leaves HEAD detached; provenance follows the cloned HEAD", func(t *testing.T) {
		runGit(t, root, "tag", "v1", shaA)
		res, err := Git(root, GitOptions{Branch: "v1"}, discard())
		if err != nil {
			t.Fatalf("Git(Branch=v1): %v", err)
		}
		if res.Branch == "" || res.Branch == "HEAD" {
			t.Errorf("Branch = %q, want a commit SHA (detached HEAD)", res.Branch)
		}
		// The clone's HEAD is the tagged (earlier) commit, so the
		// provenance follows it: every file carries the earlier
		// commit's SHA and committer time.
		if len(res.Files) == 0 {
			t.Fatal("no files discovered at v1")
		}
		for _, f := range res.Files {
			if f.CommitSHA != shaA || !f.DocUpdatedAt.Equal(timeA) {
				t.Errorf("%s at v1: CommitSHA=%q DocUpdatedAt=%v, want %q %v",
					f.RelPath, f.CommitSHA, f.DocUpdatedAt, shaA, timeA)
			}
		}
	})

	t.Run("a missing ref is a runtime error, not a configuration error", func(t *testing.T) {
		_, err := Git(root, GitOptions{Branch: "no-such-branch"}, discard())
		if err == nil {
			t.Fatal("Git(Branch=no-such-branch) succeeded, want an error")
		}
		if config.IsConfigurationError(err) {
			t.Errorf("missing ref classified as a configuration error: %v", err)
		}
	})
}

// TestGitValidation: sources that fail normalization, and malformed
// globs, wrap config.ErrConfiguration — the CLI (T7) maps them to
// exit code 2 (docs/PLAN.md §8). No git binary is needed: both fail
// before anything is executed.
func TestGitValidation(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"not a url",
		"owner",
		"owner/",
		"a/b/c",
		"ftp://x/y/z",
		"https://",
		"https://github.com/",
		"git@host",
	} {
		t.Run("source "+strconv.Quote(in), func(t *testing.T) {
			_, err := Git(in, GitOptions{}, discard())
			if err == nil {
				t.Fatalf("Git(%q) succeeded, want a configuration error", in)
			}
			if !config.IsConfigurationError(err) {
				t.Errorf("err = %v, want it to wrap config.ErrConfiguration", err)
			}
		})
	}

	for name, opts := range map[string]GitOptions{
		"invalid include glob": {Include: []string{"[.md"}},
		"invalid exclude glob": {Exclude: []string{"a{b"}},
	} {
		t.Run(name, func(t *testing.T) {
			// A valid, existing source: the failure must be the
			// glob, not the URL.
			root := t.TempDir()
			_, err := Git(root, opts, discard())
			if err == nil {
				t.Fatalf("Git with %v succeeded, want a configuration error", opts)
			}
			if !config.IsConfigurationError(err) {
				t.Errorf("err = %v, want it to wrap config.ErrConfiguration", err)
			}
		})
	}
}

// authServer is a minimal git-over-http endpoint: the first request
// (no credentials) gets a 401 challenge; a request that presents
// credentials gets a 404, so the clone fails deterministically and
// quickly — no external network. It records the last Authorization
// header it saw.
func authServer(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		lastAuth = auth
		mu.Unlock()
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastAuth
	}
}

// TestGitTokenInjection: a supplied token reaches the clone attempt —
// the fake endpoint sees it as the Basic-auth userinfo of the
// challenged request — but it never appears in the returned error or
// in any log output (docs/PLAN.md §11 #6; AGENTS.md). The second
// subtest covers the pass-through: credentials already embedded in the
// URL win over a supplied token, and neither is ever echoed.
func TestGitTokenInjection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH (a runtime dependency, docs/PLAN.md §12)")
	}

	t.Run("token injected, never leaked", func(t *testing.T) {
		const token = "s3kr3t-token"
		srv, seenAuth := authServer(t)

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		_, err := Git(srv.URL+"/owner/repo", GitOptions{Token: token}, log)
		if err == nil {
			t.Fatal("Git succeeded against a 404 endpoint, want a failure")
		}
		if config.IsConfigurationError(err) {
			t.Errorf("clone failure classified as a configuration error: %v", err)
		}

		// git answers the 401 challenge with the URL's userinfo as
		// Basic auth — the token reached the wire.
		decoded, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(seenAuth(), "Basic "))
		if derr != nil || string(decoded) != "x-access-token:"+token {
			t.Errorf("endpoint saw Authorization %q, want Basic userinfo %q", seenAuth(), "x-access-token:"+token)
		}
		for name, out := range map[string]string{"error": err.Error(), "log": buf.String()} {
			if strings.Contains(out, token) {
				t.Errorf("the token appears in the %s output:\n%s", name, out)
			}
		}
	})

	t.Run("embedded credentials pass through, token ignored, neither leaked", func(t *testing.T) {
		const token = "s3kr3t-token"
		const embedded = "bob:pass123"
		srv, seenAuth := authServer(t)

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		u, _ := url.Parse(srv.URL + "/owner/repo")
		authURL := u.Scheme + "://" + embedded + "@" + u.Host + "/owner/repo"
		_, err := Git(authURL, GitOptions{Token: token}, log)
		if err == nil {
			t.Fatal("Git succeeded against a 404 endpoint, want a failure")
		}

		decoded, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(seenAuth(), "Basic "))
		if derr != nil || string(decoded) != embedded {
			t.Errorf("endpoint saw Authorization %q, want the embedded %q", seenAuth(), embedded)
		}
		for name, out := range map[string]string{"error": err.Error(), "log": buf.String()} {
			if strings.Contains(out, token) || strings.Contains(out, embedded) {
				t.Errorf("a credential appears in the %s output:\n%s", name, out)
			}
		}
	})
}
