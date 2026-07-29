package payload

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/MetrolistGroup/MetrolistManager/internal/version"
)

const (
	manifestName        = "install-manifest.json"
	maxManifestSize     = 64 << 10
	maxExtractedSize    = 8 << 30
	maxArchiveFileCount = 100_000
	maxRelativePath     = 240
)

type Manifest struct {
	Schema       int    `json:"schema"`
	AppID        string `json:"appId"`
	Version      string `json:"version"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	Launcher     string `json:"launcher"`
}

type Archive struct {
	reader   *zip.ReadCloser
	entries  []archiveEntry
	manifest Manifest
	verified bool
}

type archiveEntry struct {
	file      *zip.File
	path      string
	directory bool
}

func Open(archivePath, expectedVersion string) (*Archive, Manifest, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("open payload: %w", err)
	}
	archive, manifest, err := inspect(reader, expectedVersion)
	if err != nil {
		reader.Close()
		return nil, Manifest{}, err
	}
	return archive, manifest, nil
}

func Inspect(archivePath, expectedVersion string) (Manifest, error) {
	archive, manifest, err := Open(archivePath, expectedVersion)
	if err != nil {
		return Manifest{}, err
	}
	if err := archive.Close(); err != nil {
		return Manifest{}, fmt.Errorf("close payload: %w", err)
	}
	return manifest, nil
}

func inspect(reader *zip.ReadCloser, expectedVersion string) (*Archive, Manifest, error) {
	if len(reader.File) > maxArchiveFileCount {
		return nil, Manifest{}, fmt.Errorf("payload contains too many files: %d", len(reader.File))
	}

	entries := make([]archiveEntry, 0, len(reader.File))
	types := make(map[string]bool, len(reader.File))
	var total uint64
	var manifestFile *zip.File
	for _, file := range reader.File {
		clean, err := safeArchivePath(file.Name)
		if err != nil {
			return nil, Manifest{}, err
		}
		key := strings.ToLower(clean)
		if _, exists := types[key]; exists {
			return nil, Manifest{}, fmt.Errorf("payload contains duplicate path %q", file.Name)
		}
		info := file.FileInfo()
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil, Manifest{}, fmt.Errorf("payload contains unsupported file type %q", file.Name)
		}
		if file.UncompressedSize64 > maxExtractedSize-total {
			return nil, Manifest{}, errors.New("payload expands beyond the 8 GiB safety limit")
		}
		total += file.UncompressedSize64
		types[key] = info.IsDir()
		entries = append(entries, archiveEntry{file: file, path: clean, directory: info.IsDir()})
		if clean == manifestName {
			manifestFile = file
		}
	}
	for name := range types {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if directory, exists := types[parent]; exists && !directory {
				return nil, Manifest{}, fmt.Errorf("payload file %q is also used as a directory", parent)
			}
		}
	}
	if manifestFile == nil {
		return nil, Manifest{}, errors.New("payload is missing install-manifest.json at its root")
	}
	if manifestFile.FileInfo().IsDir() || manifestFile.UncompressedSize64 > maxManifestSize {
		return nil, Manifest{}, errors.New("install manifest is not a valid file")
	}

	manifest, err := readManifest(manifestFile, expectedVersion)
	if err != nil {
		return nil, Manifest{}, err
	}
	launcher, ok := types[strings.ToLower(manifest.Launcher)]
	if !ok || launcher {
		return nil, Manifest{}, fmt.Errorf("payload launcher %q is missing or is not a file", manifest.Launcher)
	}
	return &Archive{reader: reader, entries: entries, manifest: manifest}, manifest, nil
}

func readManifest(file *zip.File, expectedVersion string) (Manifest, error) {
	rc, err := file.Open()
	if err != nil {
		return Manifest{}, fmt.Errorf("open install manifest: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(rc, maxManifestSize+1))
	closeErr := rc.Close()
	if readErr != nil {
		return Manifest{}, fmt.Errorf("read install manifest: %w", readErr)
	}
	if closeErr != nil {
		return Manifest{}, fmt.Errorf("close install manifest: %w", closeErr)
	}
	if len(data) > maxManifestSize {
		return Manifest{}, errors.New("install manifest is too large")
	}

	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode install manifest: %w", err)
	}
	if err := requireJSONEnd(decoder); err != nil {
		return Manifest{}, fmt.Errorf("decode install manifest: %w", err)
	}
	if err := validateManifest(manifest, expectedVersion); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (archive *Archive) Extract(destination string) error {
	if err := archive.Verify(); err != nil {
		return err
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve payload destination: %w", err)
	}
	var extracted uint64
	for _, entry := range archive.entries {
		target := filepath.Join(root, filepath.FromSlash(entry.path))
		if entry.directory {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create payload directory %q: %w", entry.path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create payload parent for %q: %w", entry.path, err)
		}
		rc, err := entry.file.Open()
		if err != nil {
			return fmt.Errorf("open payload file %q: %w", entry.path, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create payload file %q: %w", entry.path, err)
		}
		written, copyErr := io.Copy(out, io.LimitReader(rc, int64(entry.file.UncompressedSize64)+1))
		closeErr := out.Close()
		rcErr := rc.Close()
		if copyErr != nil || closeErr != nil || rcErr != nil || uint64(written) != entry.file.UncompressedSize64 {
			os.Remove(target)
			switch {
			case copyErr != nil:
				return fmt.Errorf("extract payload file %q: %w", entry.path, copyErr)
			case closeErr != nil:
				return fmt.Errorf("close payload file %q: %w", entry.path, closeErr)
			case rcErr != nil:
				return fmt.Errorf("close archived file %q: %w", entry.path, rcErr)
			default:
				return fmt.Errorf("payload file %q has an invalid expanded size", entry.path)
			}
		}
		extracted += uint64(written)
		if extracted > maxExtractedSize {
			return errors.New("payload expands beyond the 8 GiB safety limit")
		}
	}
	return nil
}

func (archive *Archive) Verify() error {
	return archive.VerifyContext(context.Background())
}

func (archive *Archive) VerifyContext(ctx context.Context) error {
	if archive == nil || archive.reader == nil {
		return errors.New("payload archive is closed")
	}
	if archive.verified {
		return nil
	}
	var total uint64
	for _, entry := range archive.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.directory {
			continue
		}
		rc, err := entry.file.Open()
		if err != nil {
			return fmt.Errorf("verify payload file %q: %w", entry.path, err)
		}
		written, readErr := io.Copy(io.Discard, io.LimitReader(&contextReader{ctx: ctx, reader: rc}, int64(entry.file.UncompressedSize64)+1))
		closeErr := rc.Close()
		if readErr != nil {
			return fmt.Errorf("verify payload file %q: %w", entry.path, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close payload file %q: %w", entry.path, closeErr)
		}
		if uint64(written) != entry.file.UncompressedSize64 {
			return fmt.Errorf("payload file %q has an invalid expanded size", entry.path)
		}
		total += uint64(written)
		if total > maxExtractedSize {
			return errors.New("payload expands beyond the 8 GiB safety limit")
		}
	}
	archive.verified = true
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func (archive *Archive) Manifest() Manifest {
	if archive == nil {
		return Manifest{}
	}
	return archive.manifest
}

func (archive *Archive) Close() error {
	if archive == nil || archive.reader == nil {
		return nil
	}
	err := archive.reader.Close()
	archive.reader = nil
	return err
}

func validateManifest(manifest Manifest, expectedVersion string) error {
	if err := version.Validate(expectedVersion); err != nil {
		return fmt.Errorf("invalid expected payload version %q: %w", expectedVersion, err)
	}
	if manifest.Schema != 1 {
		return fmt.Errorf("unsupported install manifest schema %d", manifest.Schema)
	}
	if manifest.AppID != "com.metrolist.desktop" {
		return fmt.Errorf("unexpected app ID %q", manifest.AppID)
	}
	if manifest.Version != expectedVersion {
		return fmt.Errorf("payload version %q does not match installer version %q", manifest.Version, expectedVersion)
	}
	if manifest.Platform != "windows" || manifest.Architecture != "x86_64" {
		return fmt.Errorf("payload targets %s/%s, expected windows/x86_64", manifest.Platform, manifest.Architecture)
	}
	if manifest.Launcher != "Metrolist.exe" {
		return fmt.Errorf("unexpected payload launcher %q", manifest.Launcher)
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") {
		return "", fmt.Errorf("payload contains unsafe path %q", name)
	}
	trimmed := strings.TrimSuffix(name, "/")
	for _, component := range strings.Split(trimmed, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("payload contains unsafe path %q", name)
		}
	}
	clean := path.Clean(trimmed)
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("payload contains unsafe path %q", name)
	}
	if len(utf16.Encode([]rune(clean))) > maxRelativePath {
		return "", fmt.Errorf("payload path is too long %q", name)
	}
	for _, component := range strings.Split(clean, "/") {
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return "", fmt.Errorf("payload contains unsafe Windows path %q", name)
		}
		if len(utf16.Encode([]rune(component))) > 255 {
			return "", fmt.Errorf("payload path component is too long %q", name)
		}
		for _, character := range component {
			if character < 32 || strings.ContainsRune(`<>:"|?*`, character) {
				return "", fmt.Errorf("payload contains unsafe Windows path %q", name)
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if isReservedWindowsName(base) {
			return "", fmt.Errorf("payload contains reserved Windows path %q", name)
		}
	}
	return clean, nil
}

func isReservedWindowsName(name string) bool {
	if name == "CON" || name == "PRN" || name == "AUX" || name == "NUL" || name == "CONIN$" || name == "CONOUT$" || name == "CLOCK$" {
		return true
	}
	if len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '1' && name[3] <= '9' {
		return true
	}
	return (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && len([]rune(name)) == 4 && strings.ContainsRune("¹²³", []rune(name)[3])
}

func requireJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected data after JSON object")
		}
		return err
	}
	return nil
}
