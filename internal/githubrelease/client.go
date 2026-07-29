package githubrelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxAssetSize = 8 << 30

type Asset struct {
	Name   string
	URL    string
	Digest string
	Size   int64
}

type Progress func(downloaded, total int64)

type Client struct {
	HTTPClient *http.Client
	APIBaseURL string
	UserAgent  string
}

func NewClient(userAgent string) *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 30 * time.Minute},
		APIBaseURL: "https://api.github.com",
		UserAgent:  userAgent,
	}
}

func (c *Client) FindAsset(ctx context.Context, owner, repository, tag, assetName string) (Asset, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", strings.TrimRight(c.APIBaseURL, "/"), owner, repository, tag)
	if err := requireHTTPSURL(url); err != nil {
		return Asset{}, fmt.Errorf("invalid GitHub API URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Asset{}, err
	}
	c.setHeaders(req)

	resp, err := c.client().Do(req)
	if err != nil {
		return Asset{}, fmt.Errorf("query GitHub release: %w", err)
	}
	defer resp.Body.Close()
	if err := requireHTTPSResponse(resp); err != nil {
		return Asset{}, fmt.Errorf("query GitHub release: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Asset{}, responseError("query GitHub release", resp)
	}

	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name               string `json:"name"`
			State              string `json:"state"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return Asset{}, fmt.Errorf("read GitHub release: %w", err)
	}
	if len(body) > 4<<20 {
		return Asset{}, errors.New("GitHub release response is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&release); err != nil {
		return Asset{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Asset{}, errors.New("GitHub release response contains trailing data")
		}
		return Asset{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	if release.TagName != tag || release.Draft || release.Prerelease {
		return Asset{}, fmt.Errorf("release %q is not a published stable release", tag)
	}
	for _, candidate := range release.Assets {
		if candidate.Name != assetName {
			continue
		}
		if candidate.State != "uploaded" {
			return Asset{}, fmt.Errorf("release asset %q is not ready", assetName)
		}
		if candidate.Size <= 0 || candidate.Size > maxAssetSize {
			return Asset{}, fmt.Errorf("release asset %q has invalid size %d", assetName, candidate.Size)
		}
		if _, err := parseDigest(candidate.Digest); err != nil {
			return Asset{}, fmt.Errorf("release asset %q: %w", assetName, err)
		}
		if candidate.BrowserDownloadURL == "" {
			return Asset{}, fmt.Errorf("release asset %q has no download URL", assetName)
		}
		if err := requireHTTPSURL(candidate.BrowserDownloadURL); err != nil {
			return Asset{}, fmt.Errorf("release asset %q: %w", assetName, err)
		}
		return Asset{
			Name:   candidate.Name,
			URL:    candidate.BrowserDownloadURL,
			Digest: candidate.Digest,
			Size:   candidate.Size,
		}, nil
	}
	return Asset{}, fmt.Errorf("release %q does not contain %q", tag, assetName)
}

func (c *Client) Download(ctx context.Context, asset Asset, destination string, progress Progress) error {
	expectedDigest, err := parseDigest(asset.Digest)
	if err != nil {
		return err
	}
	if asset.Size <= 0 || asset.Size > maxAssetSize {
		return fmt.Errorf("invalid expected asset size %d", asset.Size)
	}
	if err := requireHTTPSURL(asset.URL); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create download directory: %w", err)
	}
	partial := destination + ".part"
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear partial download: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if err := requireHTTPSResponse(resp); err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return responseError("download "+asset.Name, resp)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != asset.Size {
		return fmt.Errorf("download size header is %d bytes, expected %d", resp.ContentLength, asset.Size)
	}

	out, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create partial download: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hash), &progressReader{reader: io.LimitReader(resp.Body, asset.Size+1), total: asset.Size, progress: progress})
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(partial)
		return fmt.Errorf("download %s: %w", asset.Name, errors.Join(copyErr, syncErr, closeErr))
	}
	if written != asset.Size {
		os.Remove(partial)
		return fmt.Errorf("downloaded %d bytes, expected %d", written, asset.Size)
	}
	actualDigest := hex.EncodeToString(hash.Sum(nil))
	if actualDigest != expectedDigest {
		os.Remove(partial)
		return fmt.Errorf("SHA-256 mismatch for %s", asset.Name)
	}
	if err := os.Rename(partial, destination); err != nil {
		os.Remove(partial)
		return fmt.Errorf("finish download: %w", err)
	}
	return nil
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
}

func parseDigest(value string) (string, error) {
	algorithm, digest, ok := strings.Cut(value, ":")
	if !ok || algorithm != "sha256" || len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("missing or invalid GitHub SHA-256 digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("invalid GitHub SHA-256 digest: %w", err)
	}
	return strings.ToLower(digest), nil
}

func requireHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("invalid HTTPS URL %q", value)
	}
	return nil
}

func requireHTTPSResponse(response *http.Response) error {
	if response.Request == nil || response.Request.URL == nil || response.Request.URL.Scheme != "https" {
		return errors.New("server redirected to an insecure URL")
	}
	return nil
}

func responseError(action string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("%s: GitHub returned %s", action, resp.Status)
	}
	return fmt.Errorf("%s: GitHub returned %s: %s", action, resp.Status, message)
}

type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	progress   Progress
	lastReport time.Time
}

func (r *progressReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.downloaded += int64(n)
	if r.progress != nil && (r.lastReport.IsZero() || err != nil || r.downloaded == r.total || time.Since(r.lastReport) >= 100*time.Millisecond) {
		r.progress(r.downloaded, r.total)
		r.lastReport = time.Now()
	}
	return n, err
}
