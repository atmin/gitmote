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
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/atmin/gitmote/internal/hookrpc"
)

// zeroSHA is git's all-zero object id: the "absent" side of a create (old) or a
// delete (new).
const zeroSHA = "0000000000000000000000000000000000000000"

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
	for i := range commands {
		commands[i].Force = isForce(commands[i].Old, commands[i].New)
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

// isForce reports whether a ref update rewrites history: a deletion (new is the
// zero id) or a non-fast-forward (old is not an ancestor of new). A create (old
// is the zero id) is not a force. It runs `git merge-base --is-ancestor` in the
// bare repo, where receive-pack has both the existing objects and the pushed
// ones (the quarantine) visible; a non-zero exit — not-ancestor, or any git
// error — is treated as a force so the default-branch guard fails safe.
func isForce(old, new string) bool {
	switch {
	case new == zeroSHA:
		return true // delete
	case old == zeroSHA:
		return false // create
	default:
		err := exec.Command("git", "merge-base", "--is-ancestor", old, new).Run()
		return err != nil // exit 0 = ancestor (fast-forward); non-zero = non-fast-forward
	}
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
