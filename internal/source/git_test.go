package source

import (
	"testing"

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
	// still links: the blob URL is the origin's, not a public-state
	// assertion.
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
