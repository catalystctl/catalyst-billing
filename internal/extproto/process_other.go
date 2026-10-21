//go:build !unix

package extproto

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on platforms without process groups. Extensions
// there are killed individually, so a grandchild can outlive the client; that is
// a documented limitation of the non-unix build rather than a silent difference
// in protocol behaviour.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills the child process.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
