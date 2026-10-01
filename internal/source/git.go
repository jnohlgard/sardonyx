// Git repository source (docs/PLAN.md §5.2, task T5).
//
// Git (below) clones a repository shallowly — `git clone --depth 1` into
// a fresh temporary directory, removed on exit — discovers the tracked
// markdown files via `git ls-files` (the repository's own .gitignore is
// respected for free), and returns them as models.IngestedFile records
// with git provenance.
//
// The origin is normalized before cloning: https:// URLs are used as-is
// (the token-injection point is preserved), git@host: ssh URLs are
// converted to https (sard never clones over SSH), and the owner/repo
// shorthand targets github.com. A token is injected into the clone URL
// as an x-access-token; it — like credentials already embedded in the
// input URL — is never logged or echoed, and is scrubbed out of any git
// error text before it is returned.
package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"sardonyx/internal/models"
)

// NormalizedURL is the result of NormalizeURL: a git repository origin in
// cloneable https form, plus the facts the pipeline derives from it. It
// is a value type with no I/O: the same input always normalizes to the
// same output.
type NormalizedURL struct {
	// URL is the origin in cloneable form — https (or file:// for a
	// local path), no credentials. An https input keeps a trailing
	// ".git" (git accepts both spellings on the wire); ssh and
	// shorthand inputs never carry one.
	URL string
	// Host is the origin's lowercased host, e.g. "github.com" ("" for a
	// local file:// origin).
	Host string
	// Path is the origin's path: "owner/repo[.git]" for a remote
	// origin, an absolute path for a file:// origin.
	Path string
	// Repo is the origin in "owner/repo" form — the first two path
	// segments, a trailing ".git" stripped; for a file:// origin, the
	// path's basename. It becomes the records' RootLabel.
	Repo string
	// Source is the default Onyx source enum for the origin's host
	// (models.DocumentSource*): github.com → github, gitlab.* →
	// gitlab, anything else — including local file:// origins — →
	// file. A --source flag can override it at transform time.
	Source string
	// IDBase is the run's default document-ID base (docs/PLAN.md
	// §5.2, §6): the origin URL with a lowercased host, a trailing
	// ".git" stripped, and no credentials.
	IDBase string
	// Auth is the "user:password" userinfo embedded in an https input,
	// verbatim ("" if none). CloneURL passes it through to git as-is;
	// it never appears in URL, IDBase, or any log output.
	Auth string
	// Local reports a file:// origin: cloned from a local path, never
	// token-injected.
	Local bool
}

// NormalizeURL normalizes a git repository input into a cloneable https
// origin (docs/PLAN.md §5.2). It is pure: no network, no filesystem.
//
// Recognized input forms:
//
//   - "https://host/owner/repo[.git]" (http:// too) — unchanged apart
//     from a lowercased host; embedded "user:password" is split into
//     Auth and removed from the URL and the ID base.
//   - "git@host:owner/repo[.git]" — converted to "https://host/owner/
//     repo" (sard never clones over SSH; the leading user — "git",
//     "gitlab", … — carries no credential). ssh:// URLs convert the
//     same way, keeping any embedded "user:password".
//   - "owner/repo[.git]" — the GitHub shorthand for
//     "https://github.com/owner/repo".
//   - "file:///abs/path" — a local path: Local, source file.
//
// The origin's host maps to the default Onyx source enum:
// github.com → github, gitlab.com and gitlab.* subdomains → gitlab,
// anything else → file.
//
// Any other input (no path, a single path segment, an unsupported
// scheme, …) is an error; Git wraps it into config.ErrConfiguration so
// the CLI can map it to exit code 2 (docs/PLAN.md §8).
func NormalizeURL(raw string) (NormalizedURL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return NormalizedURL{}, errors.New("empty source")
	}

	if !strings.Contains(s, "://") {
		if at := strings.IndexByte(s, '@'); at != -1 {
			// scp-like ssh form: user@host:path (no "://").
			rest := s[at+1:]
			if colon := strings.IndexByte(rest, ':'); colon > 0 {
				path := strings.TrimPrefix(rest[colon+1:], "/")
				return remoteOrigin("https", rest[:colon], path, "", true)
			}
		}
		// owner/repo shorthand: exactly two non-empty segments, no
		// user part (an "@" means it was an ssh attempt, not a
		// shorthand).
		parts := strings.Split(s, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(s, "@") {
			return remoteOrigin("https", "github.com", s, "", false)
		}
		return NormalizedURL{}, fmt.Errorf(
			"unrecognized git source %q (want https://host/owner/repo, git@host:owner/repo, or owner/repo)", raw)
	}

	u, err := url.Parse(s)
	if err != nil {
		return NormalizedURL{}, fmt.Errorf("invalid URL: %v", err)
	}

	switch u.Scheme {
	case "https", "http":
		return remoteOrigin(u.Scheme, u.Host, u.EscapedPath(), userinfo(u.User), false)
	case "ssh":
		// Converted to https: the bare user (e.g. "git") is dropped;
		// an embedded "user:password" is kept for pass-through.
		return remoteOrigin("https", u.Host, u.EscapedPath(), userinfo(u.User), true)
	case "file":
		return fileOrigin(u), nil
	default:
		return NormalizedURL{}, fmt.Errorf("unsupported scheme %q: sard clones https URLs, not %s", u.Scheme, u.Scheme)
	}
}

// remoteOrigin builds a NormalizedURL for an https/http origin (or an
// ssh origin already converted to https). path is "owner/repo[...]"
// without a leading slash. When stripGit is set (ssh inputs), a
// trailing ".git" is removed from the path; https and shorthand inputs
// keep it, as git accepts both spellings on the wire.
func remoteOrigin(scheme, host, path, auth string, stripGit bool) (NormalizedURL, error) {
	host = strings.ToLower(host)
	path = strings.TrimPrefix(path, "/")
	if host == "" || host == "/" {
		return NormalizedURL{}, fmt.Errorf("URL is missing a host: %q", scheme+"://"+path)
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return NormalizedURL{}, fmt.Errorf("URL %q is missing the owner/repo path", scheme+"://"+host+"/"+path)
	}
	if stripGit {
		path = stripGitSuffix(path)
	}

	return NormalizedURL{
		URL:    scheme + "://" + host + "/" + path,
		Host:   host,
		Path:   path,
		Repo:   repoLabel(parts),
		Source: sourceForHost(host),
		IDBase: scheme + "://" + host + "/" + stripGitSuffix(path),
		Auth:   auth,
	}, nil
}

// fileOrigin normalizes a file:// URL: a local clone source (Local,
// source file, never token-injected). The path is kept as-is
// (case-sensitive) apart from a trailing ".git" in the ID base.
func fileOrigin(u *url.URL) NormalizedURL {
	host := strings.ToLower(u.Host)
	escaped := u.EscapedPath()
	repo := u.Path
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		repo = repo[i+1:]
	}
	return NormalizedURL{
		URL:    "file://" + host + escaped,
		Host:   host,
		Path:   u.Path,
		Repo:   strings.TrimSuffix(repo, ".git"),
		Source: models.DocumentSourceFile,
		IDBase: "file://" + host + stripGitSuffix(escaped),
		Local:  true,
	}
}

// userinfo extracts the "user:password" userinfo of a parsed URL, or ""
// for a bare user (e.g. the "git" in ssh://git@…) — that carries no
// secret — or no userinfo at all.
func userinfo(u *url.Userinfo) string {
	if u == nil {
		return ""
	}
	// A bare user (no password) carries no secret: it is not kept.
	if _, ok := u.Password(); !ok {
		return ""
	}
	return u.String()
}

// sourceForHost maps an origin host to the default Onyx source enum
// (docs/PLAN.md §5.2): github.com → github, gitlab.com and gitlab.*
// subdomains → gitlab, anything else → file.
func sourceForHost(host string) string {
	switch {
	case host == "github.com":
		return models.DocumentSourceGitHub
	case host == "gitlab.com" || strings.HasPrefix(host, "gitlab."):
		return models.DocumentSourceGitLab
	default:
		return models.DocumentSourceFile
	}
}

// repoLabel reduces a path to its "owner/repo" form: the first two
// segments, a trailing ".git" stripped from the second.
func repoLabel(parts []string) string {
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}

// stripGitSuffix removes a trailing ".git" from the repository's last
// path segment (…/owner/repo.git → …/owner/repo), leaving all other
// spellings intact.
func stripGitSuffix(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		last := path[i+1:]
		if s := strings.TrimSuffix(last, ".git"); s != last {
			return path[:i+1] + s
		}
		return path
	}
	return strings.TrimSuffix(path, ".git")
}

// CloneURL returns the URL to hand to `git clone`: a local origin
// as-is; a remote origin with embedded credentials passed through as-is
// (a supplied token is ignored — the embedded credentials win);
// otherwise a supplied token injected as an x-access-token userinfo, the
// form GitHub and GitLab accept for personal access tokens over https.
//
// The result can contain a secret. Never log it; redact it — and any
// error text derived from it — before anything leaves the process
// (docs/PLAN.md §11 #6; AGENTS.md).
func (n NormalizedURL) CloneURL(token string) string {
	if n.Local {
		return n.URL
	}
	scheme := strings.SplitN(n.URL, "://", 2)[0]
	if n.Auth != "" {
		return scheme + "://" + n.Auth + "@" + n.Host + "/" + n.Path
	}
	if token != "" {
		return scheme + "://x-access-token:" + token + "@" + n.Host + "/" + n.Path
	}
	return n.URL
}

// sectionLink returns the GitHub blob URL for the file at rel on the
// given branch — the document section's "link" (docs/PLAN.md §5.2, §6)
// — or "" for any non-GitHub origin, which the transform omits from the
// payload.
func (n NormalizedURL) sectionLink(branch, rel string) string {
	if n.Local || n.Source != models.DocumentSourceGitHub {
		return ""
	}
	return "https://" + n.Host + "/" + n.Repo + "/blob/" + branch + "/" + rel
}

// redact replaces every non-empty secret in text with "***". It applies
// to any git output that might echo a clone URL (which carries a token
// or embedded credentials) before that output is logged or returned in
// an error (docs/PLAN.md §11 #6; AGENTS.md).
func redact(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}
