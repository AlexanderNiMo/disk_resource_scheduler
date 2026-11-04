package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The daemon is stopped the way systemd stops it: with SIGTERM. It must go
// away cleanly and leave nothing half-built behind.
func TestTheDaemonGoesAwayOnSigterm(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "media8", "serials")
	dst := filepath.Join(root, "media5", "serials")
	mkdirAll(t, src)
	mkdirAll(t, dst)
	// A library big enough that the scan is still walking when the signal
	// arrives.
	for i := 0; i < 50; i++ {
		for j := 0; j < 20; j++ {
			writeFile(t, filepath.Join(src, fmt.Sprintf("Show.S%02d", i), fmt.Sprintf("ep%02d.mkv", j)), "hello")
		}
	}
	stateDir := filepath.Join(root, "state")
	configPath := filepath.Join(root, "config.yaml")
	writeFile(t, configPath, fmt.Sprintf(`
log:
  level: error
server:
  listen: "127.0.0.1:0"
scan:
  schedule: "@every 1s"
  min_age: "0s"
state:
  dir: %q
web:
  users:
    alice: "secret"
folders:
  %s:
    dir_type: src
    type: 2
    min_free: 900TB
  %s:
    dir_type: dst
    type: 2
`, stateDir, src, dst))

	bin := filepath.Join(root, "diskd")
	build := exec.Command("go", "build", "-o", bin, "github.com/alex/disk_resource_scheduler/cmd/diskd")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	cmd := exec.Command(bin, "--config", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(2 * time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// The daemon goes away on its own, with the code systemd expects.
		if err != nil {
			t.Fatalf("the daemon did not exit cleanly: %v", err)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the daemon did not go away")
	}

	// It was stopped while it was measuring the library. The folders it did
	// not reach would look like full disks, so a plan made of that half
	// measurement must not be left behind.
	body, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("read the state file: %v", err)
		}
		return
	}
	t.Fatalf("the interrupted scan left a plan behind: %s", body)
}
