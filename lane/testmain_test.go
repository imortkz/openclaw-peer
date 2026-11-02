package lane_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Built once per test binary: the hermetic fake (both child commands) and the worker binary.
var (
	fakeBin   string
	workerBin string
	// defaultRoot is the realpath of the default agent workspace served by the per-test fakebridge.
	defaultRoot string
)

// TestMain builds fakeacp and openclaw-peer, adds an openclaw shim to PATH,
// and prepares a real workspace (lane settings come from a per-test fakebridge).
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "ocp-lane-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fakeBin = filepath.Join(tmp, "bin", "fakeacp")
	workerBin = filepath.Join(tmp, "bin", "openclaw-peer")
	for pkg, out := range map[string]string{"./internal/fakeacp": fakeBin, "./cmd/openclaw-peer": workerBin} {
		cmd := exec.Command("go", "build", "-mod=vendor", "-o", out, pkg)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n", pkg, err)
			return 1
		}
	}
	defaultRoot = filepath.Join(tmp, "ws", "lane")
	if err := os.MkdirAll(defaultRoot, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Symlink(fakeBin, filepath.Join(tmp, "bin", "openclaw")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	stateDir := filepath.Join(tmp, "oc-state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = os.Setenv("OPENCLAW_STATE_DIR", stateDir)
	_ = os.Setenv("PATH", filepath.Join(tmp, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"FAKEACP_SCRIPT", "FAKEACP_LOG", "FAKEACP_STATE", "OPENCLAW_GATEWAY_TOKEN", "OPENCLAW_GATEWAY_PASSWORD", "OPENCLAW_GATEWAY_URL"} {
		_ = os.Unsetenv(k)
	}
	return m.Run()
}
