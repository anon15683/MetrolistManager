//go:build windows

package winplatform

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerLockIsExclusive(t *testing.T) {
	release, err := AcquireManagerLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireManagerLock(); err == nil {
		release()
		t.Fatal("second AcquireManagerLock() succeeded")
	}
	release()
	releaseAgain, err := AcquireManagerLock()
	if err != nil {
		t.Fatalf("AcquireManagerLock() after release: %v", err)
	}
	releaseAgain()
}

func TestFindWaitAndTerminateInstalledApp(t *testing.T) {
	root := t.TempDir()
	appDirectory := filepath.Join(root, "app")
	if err := os.MkdirAll(appDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(appDirectory, "Metrolist.exe")
	copyFile(t, os.Args[0], launcher)
	command := exec.Command(launcher, "-test.run=TestMetrolistHelperProcess")
	command.Env = append(os.Environ(), "METROLIST_TEST_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		exists, matches, err := ValidateAppProcess(root, uint32(command.Process.Pid))
		if err == nil && exists && matches {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ValidateAppProcess() = %v, %v, %v", exists, matches, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if exited, err := WaitForApps(root, 20*time.Millisecond); err != nil || exited {
		t.Fatalf("WaitForApps() = %v, %v; want running", exited, err)
	}
	if err := TerminateRunningApps(root); err != nil {
		t.Fatal(err)
	}
	if exited, err := WaitForApps(root, time.Second); err != nil || !exited {
		t.Fatalf("WaitForApps() = %v, %v; want exited", exited, err)
	}
}

func TestMetrolistHelperProcess(t *testing.T) {
	if os.Getenv("METROLIST_TEST_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestCreateShortcut(t *testing.T) {
	shortcut := filepath.Join(t.TempDir(), "Metrolist.lnk")
	if err := createShortcut(shortcut, os.Args[0], filepath.Dir(os.Args[0])); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(shortcut); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("shortcut stat = %v, %v", info, err)
	}
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
