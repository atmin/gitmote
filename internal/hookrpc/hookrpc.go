// Package hookrpc is the control channel between git's pre-receive hook and the
// gitmote parent process, per docs/notes/push-hook-channel.md. The hook runs as
// a child of receive-pack — a separate process that cannot touch the embedded
// single-writer s3lite — so it RPCs the parent (the sole writer) over a unix
// socket. The parent performs the S3 PUT + ref CAS and returns a verdict the
// hook turns into its exit code.
package hookrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// Environment variables the parent exports into receive-pack (and thus the
// hook): the socket to dial and the single-use nonce that binds the call to the
// in-flight push.
const (
	EnvSock  = "GITMOTE_SOCK"
	EnvNonce = "GITMOTE_NONCE"
)

// RefCommand is one `<old> <new> <ref>` line from the hook's stdin. old/new are
// object ids; the all-zero id means "absent" (create when old, delete when new).
// Force is set by the hook when the update rewrites history — a deletion, or a
// non-fast-forward (old is not an ancestor of new). The parent uses it to gate
// force-pushes of the default branch to admins (docs/architecture/urls.md); the
// hook computes it because only it sits in the repo with the objects present.
type RefCommand struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	Ref   string `json:"ref"`
	Force bool   `json:"force"`
}

// Request is the hook → parent message: the nonce, the ref commands, and the
// absolute path of the quarantine object directory holding the pushed objects.
type Request struct {
	Nonce          string       `json:"nonce"`
	Commands       []RefCommand `json:"commands"`
	QuarantinePath string       `json:"quarantine_path"`
}

// Response is the parent → hook verdict. OK=false rejects the push; Reason is
// surfaced to the client via the hook's stderr.
type Response struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Call dials the parent's unix socket, sends req, and returns its verdict. It
// is used by the hook binary.
func Call(sockPath string, req Request) (Response, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return Response{}, fmt.Errorf("hookrpc: dial %s: %w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("hookrpc: send: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("hookrpc: receive: %w", err)
	}
	return resp, nil
}

// Serve accepts connections on l until l is closed, handling each with h. Each
// connection carries exactly one Request/Response exchange.
func Serve(l net.Listener, h func(Request) Response) {
	for {
		conn, err := l.Accept()
		if err != nil {
			// A closed listener ends the loop; nothing else is recoverable.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go serveConn(conn, h)
	}
}

func serveConn(conn net.Conn, h func(Request) Response) {
	defer func() { _ = conn.Close() }()
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(Response{OK: false, Reason: "malformed request"})
		return
	}
	_ = json.NewEncoder(conn).Encode(h(req))
}
