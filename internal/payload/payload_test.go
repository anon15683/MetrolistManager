package payload

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectAndExtract(t *testing.T) {
	archive := writeZip(t, map[string]string{
		manifestName:           `{"schema":1,"appId":"com.metrolist.desktop","version":"14.0.1","platform":"windows","architecture":"x86_64","launcher":"Metrolist.exe"}`,
		"Metrolist.exe":        "launcher",
		"runtime/bin/java.exe": "runtime",
	})

	payload, manifest, err := Open(archive, "14.0.1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer payload.Close()
	if manifest.Launcher != "Metrolist.exe" {
		t.Fatalf("launcher = %q", manifest.Launcher)
	}

	destination := filepath.Join(t.TempDir(), "app")
	if err := payload.Extract(destination); err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "runtime", "bin", "java.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "runtime" {
		t.Fatalf("content = %q", content)
	}
}

func TestInspectRejectsUnsafeAndMismatchedPayloads(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name: "traversal",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1"),
				"Metrolist.exe": "launcher",
				"../outside":    "bad",
			},
			wantErr: "unsafe path",
		},
		{
			name: "embedded traversal segment",
			files: map[string]string{
				manifestName:       manifestJSON("14.0.1"),
				"Metrolist.exe":    "launcher",
				"runtime/../alias": "bad",
			},
			wantErr: "unsafe path",
		},
		{
			name: "empty path segment",
			files: map[string]string{
				manifestName:        manifestJSON("14.0.1"),
				"Metrolist.exe":     "launcher",
				"runtime//java.exe": "bad",
			},
			wantErr: "unsafe path",
		},
		{
			name: "wrong version",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.2"),
				"Metrolist.exe": "launcher",
			},
			wantErr: "does not match",
		},
		{
			name: "missing launcher",
			files: map[string]string{
				manifestName: manifestJSON("14.0.1"),
			},
			wantErr: "launcher",
		},
		{
			name: "trailing dot alias",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1"),
				"Metrolist.exe": "launcher",
				"settings.":     "bad",
			},
			wantErr: "unsafe Windows path",
		},
		{
			name: "reserved device name",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1"),
				"Metrolist.exe": "launcher",
				"con.txt":       "bad",
			},
			wantErr: "reserved Windows path",
		},
		{
			name: "reserved console alias",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1"),
				"Metrolist.exe": "launcher",
				"CONOUT$.txt":   "bad",
			},
			wantErr: "reserved Windows path",
		},
		{
			name: "reserved superscript device name",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1"),
				"Metrolist.exe": "launcher",
				"COM¹.txt":      "bad",
			},
			wantErr: "reserved Windows path",
		},
		{
			name: "file used as directory",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1"),
				"Metrolist.exe": "launcher",
				"runtime":       "file",
				"runtime/java":  "bad",
			},
			wantErr: "also used as a directory",
		},
		{
			name: "trailing manifest data",
			files: map[string]string{
				manifestName:    manifestJSON("14.0.1") + `{}`,
				"Metrolist.exe": "launcher",
			},
			wantErr: "unexpected data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Inspect(writeZip(t, tt.files), "14.0.1")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Inspect() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestInspectRejectsInvalidVersionGrammar(t *testing.T) {
	_, err := Inspect(writeZip(t, map[string]string{
		manifestName:    manifestJSON("banana"),
		"Metrolist.exe": "launcher",
	}), "banana")
	if err == nil || !strings.Contains(err.Error(), "invalid expected payload version") {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func TestVerifyRejectsCorruptFileData(t *testing.T) {
	archivePath := writeStoredZip(t, map[string]string{
		manifestName:    manifestJSON("14.0.1"),
		"Metrolist.exe": "unique-launcher-contents",
	})
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
	archive, _, err := Open(archivePath, "14.0.1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer archive.Close()
	if err := archive.Verify(); err == nil {
		t.Fatal("Verify() accepted corrupt payload data")
	}
}

func manifestJSON(version string) string {
	return `{"schema":1,"appId":"com.metrolist.desktop","version":"` + version + `","platform":"windows","architecture":"x86_64","launcher":"Metrolist.exe"}`
}

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	for name, content := range files {
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeStoredZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	for name, content := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		entry, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
