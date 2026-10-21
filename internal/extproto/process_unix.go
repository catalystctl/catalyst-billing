//go:build unix

package extproto

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so that killing the
// group also kills anything it spawned. An extension that shells out to a cloud
// CLI leaves grandchildren behind otherwise, and a leaked grandchild keeps
// holding the stdout pipe open, which looks exactly like a hang.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the child's whole process group.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	// A negative pid addresses the group whose leader is pid. It fails with
	// ESRCH once the leader has been reaped, which is not worth reporting.
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
