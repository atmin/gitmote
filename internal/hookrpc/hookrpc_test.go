package hookrpc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// listen starts a Serve loop on a short-path unix socket and returns its path.
func listen(t *testing.T, h func(Request) Response) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go Serve(l, h)
	return sock
}

func TestCallRoundTrip(t *testing.T) {
	var got Request
	sock := listen(t, func(req Request) Response {
		got = req
		return Response{OK: true}
	})

	req := Request{
		Nonce:          "n1",
		QuarantinePath: "/q",
		Commands:       []RefCommand{{Old: "0", New: "abc", Ref: "refs/heads/main"}},
	}
	resp, err := Call(sock, req)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !resp.OK {
		t.Errorf("resp.OK = false, want true")
	}
	if got.Nonce != "n1" || got.QuarantinePath != "/q" || len(got.Commands) != 1 || got.Commands[0].New != "abc" {
		t.Errorf("server received %+v, want the sent request", got)
	}
}

func TestCallRejectVerdict(t *testing.T) {
	sock := listen(t, func(Request) Response {
		return Response{OK: false, Reason: "nope"}
	})
	resp, err := Call(sock, Request{Nonce: "x"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.OK || resp.Reason != "nope" {
		t.Errorf("resp = %+v, want {false nope}", resp)
	}
}

func TestCallNoServer(t *testing.T) {
	if _, err := Call(filepath.Join(t.TempDir(), "absent.sock"), Request{}); err == nil {
		t.Error("Call to a nonexistent socket succeeded, want error")
	}
}
