//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/MetrolistGroup/MetrolistManager/internal/installer"
)

func TestParseOptionsPreservesSilentOnErrors(t *testing.T) {
	options, err := parseOptions([]string{"unknown", "--silent"})
	if err == nil {
		t.Fatal("parseOptions() error = nil")
	}
	if !options.silent {
		t.Fatal("parseOptions() lost --silent while reporting an error")
	}
}

func TestParseOptionsPreservesSilentAfterInvalidFlag(t *testing.T) {
	tests := [][]string{
		{"install", "--bogus", "--silent"},
		{"install", "--silent", "--bogus"},
		{"uninstall", "--purge-data=maybe", "--silent"},
		{"install", "--silent", "leftover"},
		{"update", "--pid", "4294967296", "--silent"},
		{"install", "--purge-data", "--silent"},
	}
	for _, arguments := range tests {
		options, err := parseOptions(arguments)
		if err == nil {
			t.Errorf("parseOptions(%q) error = nil", arguments)
			continue
		}
		if !options.silent {
			t.Errorf("parseOptions(%q) lost --silent while reporting an error", arguments)
		}
	}
}

func TestParseOptionsHonorsSilentFalse(t *testing.T) {
	options, err := parseOptions([]string{"install", "--silent=false"})
	if err != nil {
		t.Fatal(err)
	}
	if options.silent {
		t.Fatal("parseOptions() ignored --silent=false")
	}
}

func TestParseOptionsTracksExplicitBooleans(t *testing.T) {
	options, err := parseOptions([]string{"repair", "--desktop-shortcut=false", "--launch=false"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.desktopSet || options.desktopShortcut || options.launch {
		t.Fatalf("parseOptions() = %+v", options)
	}
}

func TestParseOptionsRejectsPIDOutsideUpdate(t *testing.T) {
	if _, err := parseOptions([]string{"install", "--pid", "0"}); err == nil {
		t.Fatal("parseOptions() accepted --pid for install")
	}
}

func TestParseOptionsSetsWorkerParentPID(t *testing.T) {
	options, err := parseOptions([]string{"uninstall-worker", "--install-dir", `C:\Metrolist`, "--parent-pid", "42", "--installation-id", "0123456789abcdef0123456789abcdef", "--silent"})
	if err != nil {
		t.Fatal(err)
	}
	if options.parentPID != 42 || options.runningPID != 0 || options.installationID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("parseOptions() = %+v", options)
	}
}

func TestParseOptionsRejectsIgnoredCommandFlags(t *testing.T) {
	tests := [][]string{
		{"uninstall", "--install-dir", `C:\Wrong`},
		{"uninstall", "--launch=false"},
		{"install", "--purge-data"},
		{"repair", "--parent-pid", "1"},
		{"uninstall", "--installation-id", "0123456789abcdef0123456789abcdef"},
	}
	for _, arguments := range tests {
		if _, err := parseOptions(arguments); err == nil {
			t.Errorf("parseOptions(%q) error = nil", arguments)
		}
	}
}

func TestInstalledManagerCannotRunAsUninstallWorker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	installedManager := filepath.Join(root, installer.ManagerDirectoryName, installer.ManagerExecutable)
	if err := os.MkdirAll(filepath.Dir(installedManager), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installedManager, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateUninstallWorkerPath(root, installedManager); err == nil {
		t.Fatal("validateUninstallWorkerPath() accepted the installed manager")
	}
}

func TestUninstallCleanupRejectsReinstalledApplication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Metrolist")
	managerDirectory := filepath.Join(root, installer.ManagerDirectoryName)
	if err := os.MkdirAll(filepath.Join(root, installer.AppDirectoryName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managerDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := installer.Marker{
		Schema:         1,
		AppID:          "com.metrolist.desktop",
		InstallationID: "0123456789abcdef0123456789abcdef",
		Version:        "14.0.0-a5",
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managerDirectory, "install.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateUninstallCleanupTarget(root, marker.InstallationID); err == nil {
		t.Fatal("validateUninstallCleanupTarget() accepted a reinstalled app directory")
	}
}
