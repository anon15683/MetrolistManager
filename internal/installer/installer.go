package installer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MetrolistGroup/MetrolistManager/internal/payload"
	"github.com/MetrolistGroup/MetrolistManager/internal/version"
)

const (
	AppDirectoryName     = "app"
	ManagerDirectoryName = "manager"
	ManagerExecutable    = "MetrolistManager.exe"
	markerName           = "install.json"
)

type Marker struct {
	Schema          int    `json:"schema"`
	AppID           string `json:"appId"`
	InstallationID  string `json:"installationId"`
	Version         string `json:"version"`
	DesktopShortcut bool   `json:"desktopShortcut"`
}

type ApplyRequest struct {
	ArchivePath     string
	Archive         *payload.Archive
	InstallRoot     string
	ManagerSource   string
	ExpectedVersion string
	DesktopShortcut *bool
}

func Apply(request ApplyRequest) (result Marker, resultErr error) {
	if strings.TrimSpace(request.InstallRoot) == "" {
		return Marker{}, errors.New("installation directory is empty")
	}
	if err := version.Validate(request.ExpectedVersion); err != nil {
		return Marker{}, fmt.Errorf("invalid installer version: %w", err)
	}
	root, err := filepath.Abs(request.InstallRoot)
	if err != nil {
		return Marker{}, fmt.Errorf("resolve installation directory: %w", err)
	}
	archive := request.Archive
	ownedArchive := false
	if archive == nil {
		archive, _, err = payload.Open(request.ArchivePath, request.ExpectedVersion)
		if err != nil {
			return Marker{}, err
		}
		ownedArchive = true
	} else if archive.Manifest().Version != request.ExpectedVersion {
		return Marker{}, errors.New("prepared payload version does not match the installer version")
	}
	if ownedArchive {
		defer archive.Close()
	}
	if err := archive.Verify(); err != nil {
		return Marker{}, err
	}

	marker, exists, err := inspectRoot(root)
	if err != nil {
		return Marker{}, err
	}
	if !exists {
		installationID, err := newInstallationID()
		if err != nil {
			return Marker{}, err
		}
		marker = Marker{
			Schema:         1,
			AppID:          "com.metrolist.desktop",
			InstallationID: installationID,
		}
	}
	marker.Version = request.ExpectedVersion
	if request.DesktopShortcut != nil {
		marker.DesktopShortcut = *request.DesktopShortcut
	}
	managerDirectory := filepath.Join(root, ManagerDirectoryName)
	managerDestination := filepath.Join(managerDirectory, ManagerExecutable)
	managerSource, err := openManagerSource(request.ManagerSource, managerDestination)
	if err != nil {
		return Marker{}, err
	}
	if managerSource != nil {
		defer managerSource.Close()
	}

	appDirectory := filepath.Join(root, AppDirectoryName)
	if !exists {
		defer func() {
			if resultErr != nil {
				cleanupErr := errors.Join(os.RemoveAll(appDirectory), os.RemoveAll(managerDirectory))
				resultErr = errors.Join(resultErr, cleanupErr)
			}
		}()
	}
	if err := RemoveLauncherForReplacement(root); err != nil {
		return Marker{}, fmt.Errorf("prevent Metrolist Desktop from restarting during replacement: %w", err)
	}
	if err := os.RemoveAll(appDirectory); err != nil {
		return Marker{}, fmt.Errorf("remove existing application: %w", err)
	}
	if err := os.MkdirAll(appDirectory, 0o755); err != nil {
		return Marker{}, fmt.Errorf("create application directory: %w", err)
	}
	if err := archive.Extract(appDirectory); err != nil {
		os.RemoveAll(appDirectory)
		return Marker{}, err
	}
	launcher := filepath.Join(appDirectory, "Metrolist.exe")
	info, err := os.Stat(launcher)
	if err != nil || !info.Mode().IsRegular() {
		os.RemoveAll(appDirectory)
		return Marker{}, errors.New("extracted payload does not contain a regular Metrolist.exe launcher")
	}

	if err := os.MkdirAll(managerDirectory, 0o755); err != nil {
		return Marker{}, fmt.Errorf("create manager directory: %w", err)
	}
	if managerSource != nil {
		if err := copyExecutable(managerSource, managerDestination); err != nil {
			return Marker{}, err
		}
	}
	if err := writeMarker(root, marker); err != nil {
		return Marker{}, err
	}
	return marker, nil
}

func RemoveLauncherForReplacement(root string) error {
	return removeLauncherBeforeReplacement(filepath.Join(root, AppDirectoryName, "Metrolist.exe"))
}

func ReadMarker(root string) (Marker, error) {
	file, err := os.Open(markerPath(root))
	if err != nil {
		return Marker{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	closeErr := file.Close()
	if readErr != nil {
		return Marker{}, fmt.Errorf("read installation marker: %w", readErr)
	}
	if closeErr != nil {
		return Marker{}, fmt.Errorf("close installation marker: %w", closeErr)
	}
	var marker Marker
	if len(data) > 64<<10 {
		return Marker{}, errors.New("installation marker is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return Marker{}, fmt.Errorf("decode installation marker: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Marker{}, errors.New("installation marker has trailing data")
		}
		return Marker{}, fmt.Errorf("decode installation marker: %w", err)
	}
	installationID, idErr := hex.DecodeString(marker.InstallationID)
	if marker.Schema != 1 || marker.AppID != "com.metrolist.desktop" || idErr != nil || len(installationID) != 16 || marker.Version == "" {
		return Marker{}, errors.New("installation marker is invalid")
	}
	if err := version.Validate(marker.Version); err != nil {
		return Marker{}, fmt.Errorf("installation marker has invalid version: %w", err)
	}
	return marker, nil
}

func CompareVersions(left, right string) (int, error) {
	return version.Compare(left, right)
}

func inspectRoot(root string) (Marker, bool, error) {
	marker, err := ReadMarker(root)
	if err == nil {
		return marker, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Marker{}, false, err
	}
	entries, readErr := os.ReadDir(root)
	if errors.Is(readErr, os.ErrNotExist) {
		return Marker{}, false, nil
	}
	if readErr != nil {
		return Marker{}, false, fmt.Errorf("inspect installation directory: %w", readErr)
	}
	if len(entries) != 0 {
		return Marker{}, false, errors.New("installation directory is not empty and is not owned by Metrolist")
	}
	return Marker{}, false, nil
}

func writeMarker(root string, marker Marker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	path := markerPath(root)
	temporary := path + ".new"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear temporary installation marker: %w", err)
	}
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("write installation marker: %w", err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(temporary)
		return fmt.Errorf("write installation marker: %w", errors.Join(writeErr, syncErr, closeErr))
	}
	if err := replaceFile(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("finish installation marker: %w", err)
	}
	return nil
}

func markerPath(root string) string {
	return filepath.Join(root, ManagerDirectoryName, markerName)
}

func openManagerSource(source, destination string) (*os.File, error) {
	if source == "" {
		return nil, errors.New("manager executable path is empty")
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, fmt.Errorf("open manager executable: %w", err)
	}
	sourceInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect manager executable: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("manager executable is not a regular file")
	}
	if destinationInfo, statErr := os.Stat(destination); statErr == nil {
		if !destinationInfo.Mode().IsRegular() {
			file.Close()
			return nil, errors.New("installed manager path is not a regular file")
		}
		if os.SameFile(sourceInfo, destinationInfo) {
			file.Close()
			return nil, nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		file.Close()
		return nil, fmt.Errorf("inspect installed manager: %w", statErr)
	}
	return file, nil
}

func copyExecutable(source *os.File, destination string) error {
	temporary := destination + ".new"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear temporary manager executable: %w", err)
	}
	out, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create installed manager: %w", err)
	}
	_, copyErr := io.Copy(out, source)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(temporary)
		return fmt.Errorf("copy manager executable: %w", errors.Join(copyErr, syncErr, closeErr))
	}
	if err := replaceFile(temporary, destination); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("replace installed manager: %w", err)
	}
	return nil
}

func newInstallationID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate installation ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}
