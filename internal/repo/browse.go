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

// ErrNotFound reports that a browse lookup did not resolve in the materialized
// repo. The lookup is of a ref, a tree, or a blob. Handlers map it to 404. In
// these read-only plumbing calls, a git subcommand that exits non-zero means
// "no such object or path". So it is reported as ErrNotFound. A failure to
// launch git at all surfaces as a distinct error.
var ErrNotFound = errors.New("repo: not found")

// TreeEntry is one row of a directory listing (git ls-tree --long).
type TreeEntry struct {
	Mode string // for example "100644"
	Type string // "blob", "tree", or "commit" (submodule)
	SHA  string
	Size int64  // 0 for non-blob entries (git prints "-")
	Path string // full path from the repo root, for links
	Name string // base name, for display
}

// Commit is a single entry of history. It holds the fields that the browse UI
// shows.
type Commit struct {
	SHA     string
	Author  string
	Email   string
	When    time.Time
	Subject string
	Body    string
}

// commitFormat is a git pretty-format. The unit separator (\x1f) separates its
// fields, and \x1e terminates its records. So a multi-line body never confuses
// parsing. These are the fields, in order:
//   - hash
//   - author name
//   - author email
//   - author date (strict ISO 8601)
//   - subject
//   - body
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
		// A trailing slash makes ls-tree list the contents of the directory,
		// not the directory entry itself. A lone "--" with no pathspec makes
		// ls-tree match nothing. So add the separator only for a scoped path.
		args = append(args, "--", strings.TrimSuffix(treePath, "/")+"/")
	}
	out, err := runGitOut(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	return parseTree(out), nil
}

// EntryType reports the type of the entry that p names within commit sha. It
// returns the type that ls-tree reports:
//   - "tree" for a directory.
//   - "blob" for a file.
//   - "commit" for a submodule.
//
// It returns ErrNotFound when p names no entry. An empty path is the repo
// root, which is a tree. The unified browse handler uses it to choose the
// listing or the file view. The blob handler uses it to redirect a directory
// to its tree URL.
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
	// Without a trailing slash, ls-tree reports the entry itself (one row). So
	// its type field distinguishes a directory from a file.
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

// parseTree parses the output of `git ls-tree --long`. Each line is
// "<mode> <type> <sha> <size>\t<name>".
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

// Blob reads the file at path within commit sha. It returns these:
//   - the content
//   - the size
//   - whether it looks binary (a NUL byte in the leading bytes)
//
// It returns ErrNotFound when the path names no blob.
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

// BlobSize returns the byte size of the object at path within commit sha,
// without reading its content. It does not check the object type, so a tree
// path also returns a size. It returns ErrNotFound when the path names no
// object. The raw endpoint uses it to confirm existence before streaming.
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
// the reader, which also reaps the git process. Confirm existence with
// BlobSize first, so that a missing path is a 404 before any bytes are sent.
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

// gitReadCloser wraps the stdout of a running git process. Its Close closes
// the pipe and also waits for the process to exit.
type gitReadCloser struct {
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (g *gitReadCloser) Read(p []byte) (int, error) { return g.stdout.Read(p) }

func (g *gitReadCloser) Close() error {
	_ = g.stdout.Close()
	return g.cmd.Wait()
}

// Log returns the commit history reachable from sha, newest first. It can be
// scoped to a path. It fetches limit+1 commits. If more than limit come back,
// it drops the extra commit and more is true. The caller can then show a
// truncation marker instead of a silent cap.
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

// Show returns the metadata of a single commit and its unified diff. With
// --root, the initial commit produces a diff against the empty tree, not
// nothing.
func Show(ctx context.Context, dir, sha string) (Commit, string, error) {
	if err := safeRev(sha); err != nil {
		return Commit{}, "", err
	}
	out, err := runGitOut(ctx, dir, "show", "--format="+commitFormat, "--patch", "--root", "--end-of-options", sha)
	if err != nil {
		return Commit{}, "", err
	}
	// The format ends with \x1e. The patch follows after it.
	head, diff, _ := bytes.Cut(out, []byte{0x1e})
	commits := parseCommits(append(head, 0x1e))
	if len(commits) == 0 {
		return Commit{}, "", ErrNotFound
	}
	return commits[0], strings.TrimLeft(string(diff), "\n"), nil
}

// Compare returns the commits that head adds over base, and the unified diff
// between them. This matches a GitHub compare view:
//   - The commit list is the two-dot range base..head: the commits reachable
//     from head but not from base.
//   - The diff is the three-dot base...head: head against the merge base.
//
// base and head are commit SHAs, or any rev that safeRev accepts. An unknown
// rev surfaces as ErrNotFound.
func Compare(ctx context.Context, dir, base, head string) (commits []Commit, diff string, err error) {
	if err := safeRev(base); err != nil {
		return nil, "", err
	}
	if err := safeRev(head); err != nil {
		return nil, "", err
	}
	log, err := runGitOut(ctx, dir, "log", "--format="+commitFormat, "--end-of-options", base+".."+head)
	if err != nil {
		return nil, "", err
	}
	patch, err := runGitOut(ctx, dir, "diff", "--end-of-options", base+"..."+head)
	if err != nil {
		return nil, "", err
	}
	return parseCommits(log), strings.TrimLeft(string(patch), "\n"), nil
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

// isBinary flags content as binary if a NUL byte appears in the leading bytes.
// This is the heuristic that git itself uses for a non-text blob.
func isBinary(content []byte) bool {
	const sniff = 8000
	if len(content) > sniff {
		content = content[:sniff]
	}
	return bytes.IndexByte(content, 0) >= 0
}

// safeRev rejects a rev (a ref or a sha) that git could read as a flag.
func safeRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return fmt.Errorf("repo: unsafe rev %q: %w", rev, ErrNotFound)
	}
	return nil
}

// safePath rejects a pathspec that is absolute or that escapes the repo root.
// So a browse request can never reach outside the materialized tree.
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

// runGitOut runs a git subcommand in dir and returns its stdout. In these
// read-only plumbing calls, a non-zero exit means "no such object, path or
// ref". It is reported as ErrNotFound, with the stderr of git attached. A
// failure to launch git surfaces as a plain error.
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
