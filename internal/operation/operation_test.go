package operation

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/MetrolistGroup/MetrolistManager/internal/githubrelease"
)

type localRelease struct {
	archive       string
	afterDownload func()
}

func (r localRelease) resolve(context.Context) (githubrelease.Asset, error) {
	content, err := os.ReadFile(r.archive)
	if err != nil {
		return githubrelease.Asset{}, err
	}
	hash := sha256.Sum256(content)
	return githubrelease.Asset{
		Name: "app.zip", URL: "local", Size: int64(len(content)),
		Digest: "sha256:" + hex.EncodeToString(hash[:]),
	}, nil
}

func (r localRelease) Download(_ context.Context, _ githubrelease.Asset, destination string, progress githubrelease.Progress) error {
	source, err := os.Open(r.archive)
	if err != nil {
		return err
	}
	defer source.Close()
	destinationFile, err := os.Create(destination)
	if err != nil {
		return err
	}
	written, err := io.Copy(destinationFile, source)
	if closeErr := destinationFile.Close(); err == nil {
		err = closeErr
	}
	if progress != nil {
		progress(written, written)
	}
	if r.afterDownload != nil {
		r.afterDownload()
	}
	return err
}

type recordingRegistrar struct {
	called bool
}

func (registrar *recordingRegistrar) Register(string, string, bool) error {
	registrar.called = true
	return nil
}

func TestRunWaitsInstallsAndLaunches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	if err := os.WriteFile(manager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	stopped := false
	launched := ""
	registrar := &recordingRegistrar{}
	var stages []Stage
	err := Run(context.Background(), Request{
		Version: "14.0.1", InstallRoot: root, ManagerSource: manager,
		LaunchAfter: true,
	}, Dependencies{
		ResolveAsset: localRelease{archive: writePayload(t, "14.0.1")}.resolve,
		Downloader:   localRelease{archive: writePayload(t, "14.0.1")},
		EnsureStopped: func(context.Context) error {
			stopped = true
			return nil
		},
		Registrar: registrar,
		Launch: func(path string) error {
			launched = path
			return nil
		},
	}, func(event Event) {
		stages = append(stages, event.Stage)
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !stopped {
		t.Fatal("Run() did not ensure that the app stopped")
	}
	if !registrar.called {
		t.Fatal("Run() did not register the installation")
	}
	wantLauncher := filepath.Join(root, "app", "Metrolist.exe")
	if launched != wantLauncher {
		t.Fatalf("launched = %q, want %q", launched, wantLauncher)
	}
	if len(stages) == 0 || stages[len(stages)-1] != StageComplete {
		t.Fatalf("stages = %v", stages)
	}
	if indexOfStage(stages, StageVerifying) >= indexOfStage(stages, StageWaiting) {
		t.Fatalf("payload was not verified before waiting for the app: %v", stages)
	}
}

func TestRunHonorsCancellationBeforeInstalling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	if err := os.WriteFile(manager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := writePayload(t, "14.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	downloader := localRelease{archive: archive, afterDownload: cancel}
	err := Run(ctx, Request{
		Version: "14.0.1", InstallRoot: root, ManagerSource: manager,
	}, Dependencies{
		ResolveAsset: localRelease{archive: archive}.resolve,
		Downloader:   downloader,
		Registrar:    &recordingRegistrar{},
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if _, err := os.Stat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatalf("app directory was created after cancellation, stat error = %v", err)
	}
}

func TestRunDoesNotReplaceAppWhenShutdownIsDeclined(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	os.WriteFile(manager, []byte("manager"), 0o755)
	err := Run(context.Background(), Request{
		Version: "14.0.1", InstallRoot: root, ManagerSource: manager,
	}, Dependencies{
		ResolveAsset:  localRelease{archive: writePayload(t, "14.0.1")}.resolve,
		Downloader:    localRelease{archive: writePayload(t, "14.0.1")},
		EnsureStopped: func(context.Context) error { return fmt.Errorf("shutdown declined") },
		Registrar:     &recordingRegistrar{},
	}, nil)
	if err == nil {
		t.Fatal("Run() error = nil, want running-process error")
	}
	if _, err := os.Stat(filepath.Join(root, "app")); !os.IsNotExist(err) {
		t.Fatalf("app directory was created, stat error = %v", err)
	}
}

func indexOfStage(stages []Stage, wanted Stage) int {
	for index, stage := range stages {
		if stage == wanted {
			return index
		}
	}
	return len(stages)
}

func writePayload(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	files := map[string]string{
		"install-manifest.json": fmt.Sprintf(`{"schema":1,"appId":"com.metrolist.desktop","version":%q,"platform":"windows","architecture":"x86_64","launcher":"Metrolist.exe"}`, version),
		"Metrolist.exe":         "launcher",
	}
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		entry.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
