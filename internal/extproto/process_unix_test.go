//go:build unix

package extproto

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readPIDFile waits for a fixture to publish its pid.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, 5*time.Second, "pid file "+path, func() bool {
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return false
		}
		pid = n
		return true
	})
	return pid
}

// processAlive reports whether pid is a live (non-zombie) process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	// A zombie still answers kill(pid, 0) until it is reaped, so check its state.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no /proc: trust kill(pid, 0)
	}
	fields := strings.Fields(string(data))
	if len(fields) > 2 && fields[2] == "Z" {
		return false
	}
	return true
}

// TestVersionMismatchKillsChild proves a rejected extension does not survive the
// failed handshake: it must not linger as an orphan process.
func TestVersionMismatchKillsChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	_, err := Launch(context.Background(), gatewaySpec(
		"FAKE_GATEWAY_NEGOTIATE=fail",
		"FAKE_GATEWAY_PIDFILE="+pidFile,
	))
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("got %v, want ErrVersionMismatch", err)
	}
	pid := readPIDFile(t, pidFile)
	waitFor(t, 5*time.Second, "the rejected child to be killed", func() bool {
		return !processAlive(pid)
	})
}

// TestKillMidCall proves that killing the extension while a call is in flight
// fails that call with the crashed sentinel instead of leaving it blocked.
func TestKillMidCall(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := launchGateway(t,
		"FAKE_GATEWAY_MODE=slow",
		"FAKE_GATEWAY_SLOW_MS=5000",
		"FAKE_GATEWAY_PIDFILE="+pidFile,
	)
	pid := readPIDFile(t, pidFile)

	errCh := make(chan error, 1)
	go func() {
		var desc GatewayDescribeResult
		errCh <- c.Call(context.Background(), MethodGatewayDescribe, &GatewayDescribeParams{}, &desc)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill %d: %v", pid, err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrCrashed) {
			t.Fatalf("got %v, want ErrCrashed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight call hung after the extension was killed")
	}
}

// TestCloseKillsProcessGroup proves Close kills grandchildren too: an extension
// that shells out must not leave helper processes behind, because a surviving
// grandchild keeps the stdout pipe open and looks exactly like a hang.
func TestCloseKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	grandchildPIDFile := filepath.Join(dir, "grandchild.pid")
	script := filepath.Join(dir, "extension.sh")
	// The script starts a grandchild, records its pid, then blocks. It speaks no
	// protocol: this test is about process lifetime, not about calls.
	body := fmt.Sprintf("#!/bin/sh\nsleep 30 &\necho $! > %q\nwait\n", grandchildPIDFile)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	c, err := Launch(context.Background(), Spec{
		Path:             script,
		HandshakeTimeout: 300 * time.Millisecond,
	})
	if err == nil {
		c.Close()
		t.Fatal("Launch succeeded against a script that speaks no protocol")
	}
	if !errors.Is(err, ErrTimeout) && !errors.Is(err, ErrCrashed) {
		t.Fatalf("got %v, want a timeout or crash from the failed handshake", err)
	}

	var grandchild int
	waitFor(t, 5*time.Second, "the grandchild pid file", func() bool {
		data, err := os.ReadFile(grandchildPIDFile)
		if err != nil || len(data) == 0 {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return false
		}
		grandchild = n
		return true
	})
	waitFor(t, 10*time.Second, "the grandchild to die with its process group", func() bool {
		return !processAlive(grandchild)
	})
}
