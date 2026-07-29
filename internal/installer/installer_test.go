package installer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MetrolistGroup/MetrolistManager/internal/payload"
)

func TestApplyInstallsAndReplacesAppDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	if err := os.WriteFile(manager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	desktopShortcut := true
	first := writePayload(t, "14.0.1", map[string]string{"old.txt": "old"})
	if _, err := Apply(ApplyRequest{
		ArchivePath: first, InstallRoot: root, ManagerSource: manager,
		ExpectedVersion: "14.0.1", DesktopShortcut: &desktopShortcut,
	}); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	second := writePayload(t, "14.0.2", map[string]string{"new.txt": "new"})
	if _, err := Apply(ApplyRequest{
		ArchivePath: second, InstallRoot: root, ManagerSource: filepath.Join(root, ManagerDirectoryName, ManagerExecutable),
		ExpectedVersion: "14.0.2",
	}); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, AppDirectoryName, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old payload still exists, stat error = %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, AppDirectoryName, "new.txt")); err != nil || string(content) != "new" {
		t.Fatalf("new payload content = %q, error = %v", content, err)
	}
	marker, err := ReadMarker(root)
	if err != nil || marker.Version != "14.0.2" {
		t.Fatalf("marker = %+v, error = %v", marker, err)
	}
}

func TestApplyRejectsUnownedNonEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "personal.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := filepath.Join(t.TempDir(), "manager.exe")
	os.WriteFile(manager, []byte("manager"), 0o755)
	_, err := Apply(ApplyRequest{
		ArchivePath: writePayload(t, "14.0.1", nil), InstallRoot: root,
		ManagerSource: manager, ExpectedVersion: "14.0.1",
	})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestApplyChecksManagerBeforeRemovingExistingApp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	if err := os.WriteFile(manager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ApplyRequest{
		ArchivePath: writePayload(t, "14.0.1", map[string]string{"old.txt": "old"}),
		InstallRoot: root, ManagerSource: manager, ExpectedVersion: "14.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ApplyRequest{
		ArchivePath: writePayload(t, "14.0.2", nil), InstallRoot: root,
		ManagerSource: filepath.Join(t.TempDir(), "missing.exe"), ExpectedVersion: "14.0.2",
	}); err == nil {
		t.Fatal("Apply() error = nil, want missing manager error")
	}
	content, err := os.ReadFile(filepath.Join(root, AppDirectoryName, "old.txt"))
	if err != nil || string(content) != "old" {
		t.Fatalf("existing app changed after preflight failure: content = %q, error = %v", content, err)
	}
}

func TestApplyVerifiesPayloadBeforeRemovingExistingApp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	if err := os.WriteFile(manager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ApplyRequest{
		ArchivePath: writePayload(t, "14.0.1", map[string]string{"old.txt": "old"}),
		InstallRoot: root, ManagerSource: manager, ExpectedVersion: "14.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	corrupt := writeStoredPayload(t, "14.0.2")
	data, err := os.ReadFile(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	offset := bytes.Index(data, []byte("unique-launcher-contents"))
	if offset < 0 {
		t.Fatal("launcher contents not found in stored ZIP")
	}
	data[offset] ^= 0xff
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ApplyRequest{
		ArchivePath: corrupt, InstallRoot: root, ManagerSource: manager, ExpectedVersion: "14.0.2",
	}); err == nil {
		t.Fatal("Apply() accepted a corrupt payload")
	}
	content, err := os.ReadFile(filepath.Join(root, AppDirectoryName, "old.txt"))
	if err != nil || string(content) != "old" {
		t.Fatalf("existing app changed after payload verification failure: content = %q, error = %v", content, err)
	}
}

func TestFailedFreshInstallRemainsRetryable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	manager := filepath.Join(t.TempDir(), "manager.exe")
	if err := os.WriteFile(manager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	archivePath := writeStoredPayload(t, "14.0.1")
	archive, _, err := payload.Open(archivePath, "14.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if err := archive.Verify(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	offset := bytes.Index(data, []byte("unique-launcher-contents"))
	if offset < 0 {
		t.Fatal("launcher contents not found in stored ZIP")
	}
	data[offset] ^= 0xff
	if err := os.WriteFile(archivePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ApplyRequest{
		Archive: archive, InstallRoot: root, ManagerSource: manager, ExpectedVersion: "14.0.1",
	}); err == nil {
		t.Fatal("Apply() accepted payload corrupted after preflight")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed fresh install left %d entries behind", len(entries))
	}
	if _, err := Apply(ApplyRequest{
		ArchivePath: writePayload(t, "14.0.1", nil), InstallRoot: root,
		ManagerSource: manager, ExpectedVersion: "14.0.1",
	}); err != nil {
		t.Fatalf("retry Apply() error = %v", err)
	}
}

func TestReadMarkerRejectsTrailingData(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ManagerDirectoryName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := `{"schema":1,"appId":"com.metrolist.desktop","installationId":"0123456789abcdef0123456789abcdef","version":"14.0.1","desktopShortcut":false}{}`
	if err := os.WriteFile(filepath.Join(root, ManagerDirectoryName, markerName), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMarker(root); err == nil {
		t.Fatal("ReadMarker() accepted trailing JSON")
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"14.0.1", "14.0.1", 0},
		{"14.0.2", "14.0.1", 1},
		{"13.9.9", "14.0.0", -1},
		{"14.0.0-a5", "14.0.0-a5", 0},
		{"14.0.0-a5", "14.0.0-a4", 1},
		{"14.0.0-a5", "14.0.0", -1},
		{"14.0.0", "14.0.0-a5", 1},
	}
	for _, tt := range tests {
		got, err := CompareVersions(tt.left, tt.right)
		if err != nil || got != tt.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, %v; want %d", tt.left, tt.right, got, err, tt.want)
		}
	}
	if _, err := CompareVersions("14.0.0-beta1", "14.0.0"); err == nil {
		t.Fatal("CompareVersions() accepted an unsupported prerelease")
	}
}

func writePayload(t *testing.T, version string, extra map[string]string) string {
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
	for name, content := range extra {
		files[name] = content
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

func writeStoredPayload(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	files := map[string]string{
		"install-manifest.json": fmt.Sprintf(`{"schema":1,"appId":"com.metrolist.desktop","version":%q,"platform":"windows","architecture":"x86_64","launcher":"Metrolist.exe"}`, version),
		"Metrolist.exe":         "unique-launcher-contents",
	}
	for name, content := range files {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
