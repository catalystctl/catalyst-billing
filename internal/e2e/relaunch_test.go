package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catalyst/billing/internal/extproto"
)

// The supervisor's job is to keep a crashed extension from stopping the pipeline.
// This proves the property the worker relies on: a client whose process died is
// detectable and replaceable, so one crash does not require a worker restart.
func TestCrashedExtensionIsReplaceable(t *testing.T) {
	bin := buildCrasher(t)

	launches := atomic.Int64{}
	launch := func() *extproto.Client {
		c, err := extproto.Launch(context.Background(), extproto.Spec{
			Path:        bin,
			Env:         []string{"CRASH_ON_CALL=1"},
			CallTimeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		launches.Add(1)
		return c
	}

	first := launch()
	var out map[string]any
	err := first.Call(context.Background(), "gateway.describe", struct{}{}, &out)
	if err == nil {
		t.Fatal("expected the crashing extension to fail the call")
	}
	// The client must report itself dead, which is what lets the worker replace it.
	if dead := first.Err(); dead == nil {
		t.Fatal("a client whose process died reports Err() == nil, so it can never be replaced")
	}
	_ = first.Close()

	// A replacement works and is a new process.
	second := launch()
	defer second.Close()
	if err := second.Call(context.Background(), "health", struct{}{}, &out); err != nil {
		t.Fatalf("replacement extension: %v", err)
	}
	if launches.Load() != 2 {
		t.Fatalf("%d launches, want 2", launches.Load())
	}
}

// A client that is closed reports dead, so the worker discards rather than reuses it.
func TestClosedClientReportsDead(t *testing.T) {
	bin := buildCrasher(t)
	c, err := extproto.Launch(context.Background(), extproto.Spec{Path: bin})
	if err != nil {
		t.Fatal(err)
	}
	if c.Err() != nil {
		t.Fatalf("a fresh client reports dead: %v", c.Err())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if c.Err() == nil {
		t.Fatal("a closed client reports alive, so a worker would reuse it")
	}
}

// runGoBuild compiles src into out.
func runGoBuild(t *testing.T, dir, out, src string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build crasher: %v\n%s", err, b)
	}
}

// buildCrasher compiles a fixture that exits as soon as it is asked to do work.
func buildCrasher(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(crasherSource), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "crasher")
	runGoBuild(t, dir, out, src)
	return out
}

// crasherSource is a minimal extension: it answers the handshake, then exits when
// asked to do anything real.
const crasherSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	in := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     *uint64         ` + "`json:\"id\"`" + `
			Method string          ` + "`json:\"method\"`" + `
			Params json.RawMessage ` + "`json:\"params\"`" + `
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{
					"version": 1, "name": "crasher", "publisher": "test",
					"capabilities": []string{"health"},
				},
			})
		case "health":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{"status": "ok"},
			})
		default:
			// Exit mid-call: this is the failure the worker must survive.
			os.Exit(3)
		}
	}
}
`
