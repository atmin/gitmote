// Package hookrpc is the control channel between the pre-receive hook of git
// and the gitmote parent process. The hook runs as a child of receive-pack. It
// is a separate process, and it cannot touch the embedded single-writer
// s3lite. So it calls the parent, the sole writer, over RPC on a unix socket.
// The parent performs the S3 PUT and the ref CAS. It returns a verdict, which
// the hook turns into its exit code.
package hookrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// Environment variables that the parent exports into receive-pack, and so into
// the hook. They name the socket to dial and the single-use nonce. The nonce
// binds the call to the in-flight push.
const (
	EnvSock  = "GITMOTE_SOCK"
	EnvNonce = "GITMOTE_NONCE"
)

// RefCommand is one `<old> <new> <ref>` line from the stdin of the hook.
//
// old and new are object ids. The all-zero id means "absent". As old, it means
// a create. As new, it means a delete.
//
// The hook sets Force when the update rewrites history. That is a deletion, or
// a non-fast-forward (old is not an ancestor of new). The hook computes Force,
// because only the hook sits in the repo with the objects present.
//
// The parent uses Force to allow force-pushes of the default branch to admins
// only (docs/architecture/urls.md).
type RefCommand struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	Ref   string `json:"ref"`
	Force bool   `json:"force"`
}

// Request is the message from the hook to the parent. It holds these:
//   - the nonce
//   - the ref commands
//   - the absolute path of the quarantine object directory, which holds the
//     pushed objects
type Request struct {
	Nonce          string       `json:"nonce"`
	Commands       []RefCommand `json:"commands"`
	QuarantinePath string       `json:"quarantine_path"`
}

// Response is the verdict from the parent to the hook. OK=false rejects the
// push. Reason reaches the client through the stderr of the hook.
type Response struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Call dials the unix socket of the parent, sends req, and returns the
// verdict. The hook binary uses it.
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
			// A closed listener ends the loop. The loop skips any other accept error.
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
