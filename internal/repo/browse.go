package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound reports that a browse lookup — a ref, a tree, or a blob — did not
// resolve in the materialized repo. Handlers map it to 404. A git subcommand
// that exits non-zero on these read-only plumbing calls means "no such
// object/path", so it is reported as ErrNotFound; a failure to launch git at
// all surfaces as a distinct error.
var ErrNotFound = errors.New("repo: not found")

// TreeEntry is one row of a directory listing (git ls-tree --long).
type TreeEntry struct {
	Mode string // e.g. "100644"
	Type string // "blob", "tree", or "commit" (submodule)
	SHA  string
	Size int64  // 0 for non-blob entries (git prints "-")
	Path string // full path from the repo root, for links
	Name string // base name, for display
}

// Commit is a single entry of history: the fields the browse UI shows.
type Commit struct {
	SHA     string
	Author  string
	Email   string
	When    time.Time
	Subject string
	Body    string
}

// commitFormat is a git pretty-format whose fields are unit-separated (\x1f) and
// whose records are terminated by \x1e, so a multi-line body never confuses
// parsing. Fields, in order: hash, author name, author email, author date
// (strict ISO 8601), subject, body.
const commitFormat = "%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1e"

// ResolveRef resolves ref (a branch, tag, or hash) to a commit SHA in the
// materialized repo at dir. It rejects a ref beginning with "-" (flag
// injection) and returns ErrNotFound for an unknown ref.
func ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	if err := safeRev(ref); err != nil {
		return "", err
	}
	out, err := runGitOut(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Tree lists the immediate children of path within the commit sha. An empty
// path lists the repo root. Entries carry mode, type, sha, size, and name.
func Tree(ctx context.Context, dir, sha, treePath string) ([]TreeEntry, error) {
	if err := safeRev(sha); err != nil {
		return nil, err
	}
	if err := safePath(treePath); err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "--long", "--end-of-options", sha}
	if treePath != "" {
		// A trailing slash makes ls-tree list the directory's contents rather
		// than the directory entry itself. A lone "--" with no pathspec makes
		// ls-tree match nothing, so only add the separator when scoping.
		args = append(args, "--", strings.TrimSuffix(treePath, "/")+"/")
	}
	out, err := runGitOut(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	return parseTree(out), nil
}

// EntryType reports whether p names a "tree" or a "blob" within commit sha,
// returning ErrNotFound when it names neither. An empty path is the repo root, a
// tree. It lets the unified browse handler choose the listing vs the file view,
// and the blob handler redirect a directory to its tree URL.
func EntryType(ctx context.Context, dir, sha, p string) (string, error) {
	if p == "" {
		return "tree", nil
	}
	if err := safeRev(sha); err != nil {
		return "", err
	}
	if err := safePath(p); err != nil {
		return "", err
	}
	// Without a trailing slash, ls-tree reports the entry itself (one row), so its
	// type field distinguishes a directory from a file.
	out, err := runGitOut(ctx, dir, "ls-tree", "--long", "--end-of-options", sha, "--", strings.TrimSuffix(p, "/"))
	if err != nil {
		return "", err
	}
	entries := parseTree(out)
	if len(entries) == 0 {
		return "", ErrNotFound
	}
	return entries[0].Type, nil
}

// parseTree parses `git ls-tree --long` output: "<mode> <type> <sha> <size>\t<name>".
func parseTree(out []byte) []TreeEntry {
	var entries []TreeEntry
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		meta, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 4 {
			continue
		}
		var size int64
		if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
			size = n
		}
		entries = append(entries, TreeEntry{
			Mode: fields[0],
			Type: fields[1],
			SHA:  fields[2],
			Size: size,
			Path: name,
			Name: path.Base(name),
		})
	}
	return entries
}

// Blob reads the file at path within commit sha, returning its content, size,
// and whether it looks binary (a NUL byte in the leading bytes). It returns
// ErrNotFound when the path names no blob.
func Blob(ctx context.Context, dir, sha, blobPath string) (content []byte, size int64, binary bool, err error) {
	if err := safeRev(sha); err != nil {
		return nil, 0, false, err
	}
	if err := safePath(blobPath); err != nil {
		return nil, 0, false, err
	}
	out, err := runGitOut(ctx, dir, "cat-file", "blob", "--end-of-options", sha+":"+blobPath)
	if err != nil {
		return nil, 0, false, err
	}
	return out, int64(len(out)), isBinary(out), nil
}

// BlobSize returns the byte size of the blob at path within commit sha without
// reading its content, and ErrNotFound when the path names no blob. The raw
// endpoint uses it to confirm existence before streaming.
func BlobSize(ctx context.Context, dir, sha, blobPath string) (int64, error) {
	if err := safeRev(sha); err != nil {
		return 0, err
	}
	if err := safePath(blobPath); err != nil {
		return 0, err
	}
	out, err := runGitOut(ctx, dir, "cat-file", "-s", "--end-of-options", sha+":"+blobPath)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// BlobReader streams the blob at path within commit sha. The caller must Close
// the reader, which also reaps the git process. Existence should be confirmed
// with BlobSize first, so a missing path is a 404 before any bytes are sent.
func BlobReader(ctx context.Context, dir, sha, blobPath string) (io.ReadCloser, error) {
	if err := safeRev(sha); err != nil {
		return nil, err
	}
	if err := safePath(blobPath); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", "--end-of-options", sha+":"+blobPath)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &gitReadCloser{stdout: stdout, cmd: cmd}, nil
}

// gitReadCloser wraps a running git process's stdout so Close both closes the
// pipe and waits for the process to exit.
type gitReadCloser struct {
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (g *gitReadCloser) Read(p []byte) (int, error) { return g.stdout.Read(p) }

func (g *gitReadCloser) Close() error {
	_ = g.stdout.Close()
	return g.cmd.Wait()
}

// Log returns the commit history reachable from sha, optionally scoped to a
// path, newest first. It fetches limit+1 commits: if more than limit come back
// the extra is dropped and more is true, so the caller can show a truncation
// marker instead of silently capping.
func Log(ctx context.Context, dir, sha, logPath string, limit int) (commits []Commit, more bool, err error) {
	if err := safeRev(sha); err != nil {
		return nil, false, err
	}
	if err := safePath(logPath); err != nil {
		return nil, false, err
	}
	args := []string{"log", "--format=" + commitFormat, "-n", strconv.Itoa(limit + 1), "--end-of-options", sha}
	if logPath != "" {
		args = append(args, "--", logPath)
	}
	out, err := runGitOut(ctx, dir, args...)
	if err != nil {
		return nil, false, err
	}
	commits = parseCommits(out)
	if len(commits) > limit {
		return commits[:limit], true, nil
	}
	return commits, false, nil
}

// Show returns a single commit's metadata and its unified diff. --root makes the
// initial commit produce a diff against the empty tree rather than nothing.
func Show(ctx context.Context, dir, sha string) (Commit, string, error) {
	if err := safeRev(sha); err != nil {
		return Commit{}, "", err
	}
	out, err := runGitOut(ctx, dir, "show", "--format="+commitFormat, "--patch", "--root", "--end-of-options", sha)
	if err != nil {
		return Commit{}, "", err
	}
	// The format ends with \x1e; the patch follows after it.
	head, diff, _ := bytes.Cut(out, []byte{0x1e})
	commits := parseCommits(append(head, 0x1e))
	if len(commits) == 0 {
		return Commit{}, "", ErrNotFound
	}
	return commits[0], strings.TrimLeft(string(diff), "\n"), nil
}

// parseCommits splits git output framed by commitFormat into commits.
func parseCommits(out []byte) []Commit {
	var commits []Commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		fields := strings.Split(rec, "\x1f")
		if len(fields) < 6 {
			continue
		}
		when, _ := time.Parse(time.RFC3339, fields[3])
		commits = append(commits, Commit{
			SHA:     fields[0],
			Author:  fields[1],
			Email:   fields[2],
			When:    when,
			Subject: fields[4],
			Body:    strings.TrimRight(fields[5], "\n"),
		})
	}
	return commits
}

// isBinary flags content as binary if a NUL byte appears in the leading bytes,
// git's own heuristic for a non-text blob.
func isBinary(content []byte) bool {
	const sniff = 8000
	if len(content) > sniff {
		content = content[:sniff]
	}
	return bytes.IndexByte(content, 0) >= 0
}

// safeRev rejects a rev (ref or sha) that could be read as a git flag.
func safeRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return fmt.Errorf("repo: unsafe rev %q: %w", rev, ErrNotFound)
	}
	return nil
}

// safePath rejects a pathspec that is absolute or escapes the repo root, so a
// browse request can never reach outside the materialized tree.
func safePath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("repo: unsafe path %q: %w", p, ErrNotFound)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("repo: unsafe path %q: %w", p, ErrNotFound)
		}
	}
	return nil
}

// runGitOut runs a git subcommand in dir and returns its stdout. A non-zero
// exit — the "no such object/path/ref" case for these read-only plumbing calls
// — is reported as ErrNotFound with git's stderr attached; a failure to launch
// git surfaces as a plain error.
func runGitOut(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git %s: %s: %w",
				strings.Join(args, " "), strings.TrimSpace(stderr.String()), ErrNotFound)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}
