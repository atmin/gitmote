// Command gitmote-hook is git's pre-receive hook for gitmote. receive-pack runs
// it (via core.hooksPath) as a child process; it forwards the ref commands and
// the quarantine path to the parent over a unix socket and turns the parent's
// verdict into its exit code. It fails closed: any error rejects the push.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/atmin/gitmote/internal/hookrpc"
)

func main() {
	if err := run(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "gitmote:", err)
		os.Exit(1)
	}
}

func run(stdin io.Reader) error {
	sock := os.Getenv(hookrpc.EnvSock)
	nonce := os.Getenv(hookrpc.EnvNonce)
	if sock == "" || nonce == "" {
		return fmt.Errorf("hook invoked outside a gitmote push (%s/%s unset)", hookrpc.EnvSock, hookrpc.EnvNonce)
	}

	// receive-pack points GIT_OBJECT_DIRECTORY at the quarantine while the hook
	// runs, so it names exactly the newly pushed objects. Resolve it against the
	// hook's cwd (the bare repo) to an absolute path for the parent.
	quarantine := os.Getenv("GIT_OBJECT_DIRECTORY")
	if quarantine == "" {
		return fmt.Errorf("GIT_OBJECT_DIRECTORY unset")
	}
	quarantine, err := filepath.Abs(quarantine)
	if err != nil {
		return err
	}

	commands, err := parseCommands(stdin)
	if err != nil {
		return err
	}

	resp, err := hookrpc.Call(sock, hookrpc.Request{
		Nonce:          nonce,
		Commands:       commands,
		QuarantinePath: quarantine,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("push rejected: %s", resp.Reason)
	}
	return nil
}

// parseCommands reads the `<old> <new> <ref>` lines git feeds a pre-receive hook.
func parseCommands(r io.Reader) ([]hookrpc.RefCommand, error) {
	var commands []hookrpc.RefCommand
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed ref command %q", sc.Text())
		}
		commands = append(commands, hookrpc.RefCommand{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return commands, nil
}
