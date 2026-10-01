// Git repository source (docs/PLAN.md §5.2, task T5).
//
// Git (below) clones a repository shallowly — `git clone --depth 1` into
// a fresh temporary directory, removed on exit — discovers the tracked
// Markdown files via `git ls-files` (the repository's own .gitignore is
// respected for free), and returns them as models.IngestedFile records
// with git provenance.
//
// Provenance semantics in a shallow clone: the depth-1 clone contains
// only the HEAD commit it resolved to, so the per-file `git log -1`
// (docs/PLAN.md §5.2) attributes *every* file to that commit's SHA and
// committer time — a safe upper bound on the file's true last-modified
// time (never stale, at worst conservative for Onyx's freshness check).
// The per-file `git log` is the general form: deepening the clone in
// the future yields exact per-file provenance without further changes.
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
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

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
		if _, after, ok := strings.Cut(s, "@"); ok {
			// scp-like ssh form: user@host:path (no "://").
			rest := after
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
		if s, ok := strings.CutSuffix(last, ".git"); ok {
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
	scheme, _, _ := strings.Cut(n.URL, "://")
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

// GitOptions configures Git.
//
// The filter fields mirror LocalOptions exactly — a deliberate decision
// (docs/PLAN.md §5.2/§5.3): the include/exclude doublestar globs match
// case-sensitively against the file path relative to the repository
// root with forward slashes (`*` stays within one level, `**` crosses
// levels), MaxDepth counts path components (depth 1 is directly under
// the root; 0 is unlimited), and MaxFileSizeKiB = 0 is unlimited. The
// pipeline (task T7) therefore treats both sources uniformly.
//
// The git-specific fields:
//
//   - Branch: the ref to clone (--branch); empty is the repository's
//     default branch.
//   - Token: a git auth token for private repositories; injected into
//     the clone URL as x-access-token. Never logged or echoed.
type GitOptions struct {
	Include        []string
	Exclude        []string
	MaxDepth       int
	MaxFileSizeKiB int
	Branch         string
	Token          string
}

// GitResult is Git's output, mirroring LocalResult: the discovered
// files, sorted by RelPath; the run's default ID base — the normalized
// origin URL, lowercased host, no trailing .git, never credentials
// (docs/PLAN.md §5.2, §6); the branch the clone actually resolved to
// (used in GitHub blob URLs; a short SHA when HEAD is detached, e.g.
// a cloned tag); and Skipped, the number of files dropped by a
// per-file check (oversize, binary, empty, unreadable, no commit
// metadata; each a warning) and reported in the run summary
// (docs/PLAN.md §8). Files outside the scope of the depth, include, or
// exclude filters are not skipped — that is deliberate scoping. The
// pipeline uses IDBase as the document-ID base when no --id-base /
// SARD_ID_BASE override is set.
type GitResult struct {
	IDBase  string
	Branch  string
	Files   []models.IngestedFile
	Skipped int
}

// Git clones the repository at src (docs/PLAN.md §5.2, §5.4) and
// returns its Markdown files (.md, .mdx, .markdown) as
// models.IngestedFile records.
//
// src is one of NormalizeURL's input forms — an https URL, a git@ ssh
// URL, an owner/repo shorthand, or a file:///path URL — or a path to an
// existing local directory, which is cloned via file:// (tests use
// this for fixture repositories; an existing path wins over the
// owner/repo shorthand).
//
// Cloning runs `git clone --depth 1 [--branch <ref>] <url> <tmp>` into
// a fresh temporary directory that is removed on exit. Discovery uses
// `git ls-files` on the clone (tracked files only — the repository's
// own .gitignore is respected for free). Each file's provenance is
// obtained per file via `git log -1 -- <path>` (docs/PLAN.md §5.2):
// because the depth-1 clone contains only the resolved HEAD commit,
// every file is attributed to that commit — CommitSHA and
// DocUpdatedAt (UTC) — a safe upper bound on the file's true
// last-modified time (see the file comment). Files that are oversize,
// binary (NUL bytes), or empty are skipped with a warning and counted
// in GitResult.Skipped, as in Local; UTF-8 repair applies the same way.
//
// Record shape: Kind=git, RootLabel=the origin's "owner/repo" (or the
// path's basename for a local clone), RelPath slash-separated,
// DocUpdatedAt=the resolved HEAD commit's committer time (UTC),
// CommitSHA=the resolved HEAD commit's SHA, BlobURL=the GitHub blob
// URL for github.com origins and "" otherwise (the transform omits the
// link when empty).
//
// Error classes (the CLI maps them per docs/PLAN.md §8): an invalid
// source or a malformed glob wraps config.ErrConfiguration (exit 2);
// clone and git failures — network, authentication, a missing ref, a
// missing git binary — are ordinary runtime errors (exit 1). The token
// and any embedded credentials never appear in a returned error or in
// a log line.
//
// A nil log uses slog.Default().
func Git(src string, opts GitOptions, log *slog.Logger) (*GitResult, error) {
	if log == nil {
		log = slog.Default()
	}

	s := strings.TrimSpace(src)
	if s == "" {
		return nil, configError("invalid git source: empty (want https://host/owner/repo, git@host:owner/repo, or owner/repo)")
	}

	// A path to an existing local directory is cloned via file://… —
	// the local-transport clone honors --depth, whereas a bare path
	// clone warns that it ignores it.
	if fi, err := os.Stat(s); err == nil && fi.IsDir() {
		abs, err := filepath.Abs(s)
		if err != nil {
			return nil, configError("invalid git source %q: %v", src, err)
		}
		s = (&url.URL{Scheme: "file", Path: abs}).String()
	}

	norm, err := NormalizeURL(s)
	if err != nil {
		return nil, configError("invalid git source %q: %v", src, err)
	}

	patterns := append(append([]string{}, opts.Include...), opts.Exclude...)
	for _, pattern := range patterns {
		if !doublestar.ValidatePattern(pattern) {
			return nil, configError("invalid glob %q", pattern)
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("the git source requires the git binary, which is not on PATH")
	}

	tmp, err := os.MkdirTemp("", "sard-clone-")
	if err != nil {
		return nil, fmt.Errorf("creating a temporary clone directory: %v", err)
	}
	defer os.RemoveAll(tmp)

	// The clone URL can carry a secret (a token or embedded
	// credentials); every error from here on redacts both.
	secrets := []string{opts.Token, norm.Auth}
	cloneURL := norm.CloneURL(opts.Token)

	args := []string{"clone", "--depth", "1"}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	}
	args = append(args, cloneURL, tmp)

	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("cloning %s: %s", norm.URL, redact(strings.TrimSpace(string(out)), secrets...))
	}

	branch := resolveBranch(tmp)
	log.Info("cloned repository", "origin", norm.URL, "branch", branch)

	candidates, err := gitListMarkdown(tmp)
	if err != nil {
		return nil, fmt.Errorf("discovering files in the clone: %s", redact(err.Error(), secrets...))
	}

	files, skipped := collectGitFiles(tmp, norm, branch, candidates, opts, log, secrets)
	return &GitResult{
		IDBase:  norm.IDBase,
		Branch:  branch,
		Files:   files,
		Skipped: skipped,
	}, nil
}

// resolveBranch returns the branch the clone's HEAD points to — the
// output of `rev-parse --abbrev-ref HEAD` — or, when HEAD is detached
// (e.g. a cloned tag), the short commit SHA, which also works in a
// GitHub blob URL.
func resolveBranch(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "HEAD"
	}
	branch := strings.TrimSpace(string(out))
	if branch == "HEAD" {
		if short, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output(); err == nil {
			return strings.TrimSpace(string(short))
		}
	}
	return branch
}

// lastCommit returns what `git log -1 -- <path>` reports in the clone
// for rel — the latest commit in the clone's visible history touching
// the path (with a depth-1 clone, the resolved HEAD commit for every
// file) — plus that commit's committer time (UTC), which becomes the
// file's DocUpdatedAt (docs/PLAN.md §5.2). One `git log` call per file
// is acceptable for v1; batching is a future optimization.
func lastCommit(dir, rel string) (string, time.Time, error) {
	// %x00 puts a NUL between the SHA and the unix timestamp, so
	// neither can be confused for part of the other.
	cmd := exec.Command("git", "-c", "core.quotePath=off", "-C", dir,
		"log", "-1", "--format=%H%x00%ct", "--", rel)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", time.Time{}, fmt.Errorf("git log: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", time.Time{}, err
	}
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "\x00", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("unexpected git log output %q", line)
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parsing commit time: %v", err)
	}
	return parts[0], time.Unix(ts, 0).UTC(), nil
}

// collectGitFiles turns candidate relative paths into git
// IngestedFile records, mirroring collectFiles: files out of scope for
// MaxDepth, Include, or Exclude are dropped silently (deliberate
// scoping) while oversize, binary, and empty skips are warnings. The
// returned count is the number of files dropped by a per-file check
// (the "skipped" of the run summary, docs/PLAN.md §8). The files are
// sorted by RelPath.
func collectGitFiles(root string, norm NormalizedURL, branch string, candidates []string, opts GitOptions, log *slog.Logger, secrets []string) ([]models.IngestedFile, int) {
	var files []models.IngestedFile
	var skipped int
	maxBytes := int64(opts.MaxFileSizeKiB) * 1024
	for _, rel := range candidates {
		if opts.MaxDepth > 0 && pathDepth(rel) > opts.MaxDepth {
			continue
		}
		if !passesInclude(rel, opts.Include) {
			continue
		}
		if matchesAny(rel, opts.Exclude) {
			continue
		}

		path := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Stat(path)
		if err != nil {
			skipped++
			log.Warn("skipping unreadable file", "path", rel, "error", err)
			continue
		}
		if maxBytes > 0 && fi.Size() > maxBytes {
			skipped++
			log.Warn("skipping file larger than max file size", "path", rel,
				"sizeKiB", fi.Size()/1024, "maxKiB", opts.MaxFileSizeKiB)
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			skipped++
			log.Warn("skipping unreadable file", "path", rel, "error", err)
			continue
		}
		if bytes.IndexByte(raw, 0) >= 0 {
			skipped++
			log.Warn("skipping binary file (contains NUL bytes)", "path", rel)
			continue
		}
		content := repairUTF8(raw)
		if strings.TrimSpace(content) == "" {
			// "Empty" means zero-length or whitespace-only content.
			skipped++
			log.Warn("skipping empty file (zero-length or whitespace-only)", "path", rel)
			continue
		}

		sha, committedAt, err := lastCommit(root, rel)
		if err != nil {
			skipped++
			log.Warn("skipping file without commit metadata", "path", rel,
				"error", redact(err.Error(), secrets...))
			continue
		}

		files = append(files, models.IngestedFile{
			Kind:         models.KindGit,
			RootLabel:    norm.Repo,
			RelPath:      rel,
			Content:      content,
			DocUpdatedAt: committedAt,
			CommitSHA:    sha,
			BlobURL:      norm.sectionLink(branch, rel),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, skipped
}
