//go:build windows

package installer

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveLauncherBlocksRunningExecutable(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "Metrolist.exe")
	copyTestExecutable(t, launcher)
	command := exec.Command(launcher, "-test.run=TestLauncherHelperProcess")
	command.Env = append(os.Environ(), "METROLIST_LAUNCHER_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := removeLauncherBeforeReplacement(launcher); err == nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		t.Fatal("removeLauncherBeforeReplacement() removed a running launcher")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = command.Process.Wait()
	if err := removeLauncherBeforeReplacement(launcher); err != nil {
		t.Fatalf("removeLauncherBeforeReplacement() after exit: %v", err)
	}
}

func TestLauncherHelperProcess(t *testing.T) {
	if os.Getenv("METROLIST_LAUNCHER_HELPER") == "1" {
		time.Sleep(30 * time.Second)
	}
}

func copyTestExecutable(t *testing.T, destination string) {
	t.Helper()
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
}
