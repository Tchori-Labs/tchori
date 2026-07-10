// Package registry downloads, verifies, and locates Terraform-protocol
// provider binaries from an OpenTofu-compatible provider registry
// (https://opentofu.org/docs/internals/provider-registry-protocol/).
package registry

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultBaseURL is used by Install when baseURL is empty.
const defaultBaseURL = "https://registry.opentofu.org"

// maxZipEntryBytes caps how much of a single zip entry Install will copy to
// disk during extraction. Provider binaries are a few tens of MB at most;
// this bound exists purely to give io.Copy an upper limit so a malicious or
// corrupt archive cannot be used to exhaust disk space (G110
// decompression-bomb guard).
const maxZipEntryBytes = 1 << 30 // 1 GiB

// defaultCacheDir returns ~/.tchori/providers, used by Install when
// cacheDir is empty.
func defaultCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("registry: resolving home directory: %w", err)
	}
	return filepath.Join(home, ".tchori", "providers"), nil
}

// parseSource splits "namespace/name" into its two parts.
func parseSource(source string) (namespace, name string, err error) {
	parts := strings.Split(source, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("registry: invalid provider source %q, want NAMESPACE/NAME", source)
	}
	return parts[0], parts[1], nil
}

// versionsResponse is the shape of GET /v1/providers/{ns}/{name}/versions.
type versionsResponse struct {
	Versions []struct {
		Version string `json:"version"`
	} `json:"versions"`
}

// downloadResponse is the shape of
// GET /v1/providers/{ns}/{name}/{version}/download/{os}/{arch}.
type downloadResponse struct {
	Filename    string `json:"filename"`
	DownloadURL string `json:"download_url"`
	ShasumsURL  string `json:"shasums_url"`
	Shasum      string `json:"shasum"`
}

// httpGet issues a GET request and returns the response with its body still
// open (the caller must close it) once the status is 2xx. Non-2xx
// responses are treated as hard failures: registry error bodies are not
// guaranteed to be JSON (a 404 can come back as an HTML page from the CDN
// in front of the registry), so no attempt is made to parse a structured
// error out of them.
func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // url is built from a fixed base + validated path segments, never raw user input
	if err != nil {
		return nil, fmt.Errorf("registry: building request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("registry: GET %s: unexpected status %s", url, resp.Status)
	}
	return resp, nil
}

// findSHA256 scans a SHA256SUMS document (lines of "<hex-sha256>  <filename>")
// for the entry matching filename and returns its hash.
func findSHA256(sumsBody, filename string) (string, error) {
	for _, line := range strings.Split(sumsBody, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == filename {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("registry: no SHA256SUMS entry for %s", filename)
}

// extractZipFile copies one zip entry's content to destPath. The copy is
// bounded by maxZipEntryBytes to guard against decompression-bomb archives;
// entry names have already been validated by the caller to rule out zip-slip
// (absolute paths / ".." traversal) before this is called.
func extractZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("registry: opening %s in zip: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	// 0o600: the executable bit is applied separately by the caller once
	// extraction succeeds (see Install's os.Chmod after this returns).
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: destPath is cacheDir joined with validated ns/name/version and a safeZipEntryName-checked flat entry name, not attacker input
	if err != nil {
		return fmt.Errorf("registry: creating %s: %w", destPath, err)
	}
	if _, err := io.CopyN(out, rc, maxZipEntryBytes); err != nil && err != io.EOF {
		_ = out.Close()
		return fmt.Errorf("registry: writing %s: %w", destPath, err)
	}
	return out.Close()
}

// safeZipEntryName validates that a zip entry's name is a plain relative
// filename with no directory traversal or absolute-path component, and
// returns its base name. The real OpenTofu provider archives are always
// flat (binary + LICENSE/README/CHANGELOG at the zip root), but zip
// archives are untrusted input in general, so entries are rejected rather
// than silently sanitized (zip-slip guard).
func safeZipEntryName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("registry: empty zip entry name")
	}
	cleaned := filepath.Clean(name)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, string(filepath.Separator)+"..") {
		return "", fmt.Errorf("registry: unsafe zip entry name %q", name)
	}
	if strings.ContainsRune(cleaned, filepath.Separator) || strings.ContainsRune(cleaned, '/') {
		return "", fmt.Errorf("registry: unsafe zip entry name %q, want a flat root entry", name)
	}
	return cleaned, nil
}

// Install downloads source@version for the current GOOS/GOARCH from the
// OpenTofu registry (baseURL default "https://registry.opentofu.org"),
// verifies the zip's SHA256 against the SHA256SUMS document, extracts the
// provider binary to cacheDir/<ns>/<name>/<version>/<os>_<arch>/, and
// returns the binary path. cacheDir default: ~/.tchori/providers.
func Install(ctx context.Context, source, version, baseURL, cacheDir string) (string, error) {
	ns, name, err := parseSource(source)
	if err != nil {
		return "", err
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if cacheDir == "" {
		cacheDir, err = defaultCacheDir()
		if err != nil {
			return "", err
		}
	}

	// 1. GET versions and validate the requested version is actually offered.
	versionsURL := fmt.Sprintf("%s/v1/providers/%s/%s/versions", baseURL, ns, name)
	vResp, err := httpGet(ctx, versionsURL)
	if err != nil {
		return "", err
	}
	var vBody versionsResponse
	decErr := json.NewDecoder(vResp.Body).Decode(&vBody)
	_ = vResp.Body.Close()
	if decErr != nil {
		return "", fmt.Errorf("registry: decoding %s: %w", versionsURL, decErr)
	}
	found := false
	for _, v := range vBody.Versions {
		if v.Version == version {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("registry: %s/%s has no version %q", ns, name, version)
	}

	// 2. GET the download descriptor for this GOOS/GOARCH.
	downloadURL := fmt.Sprintf("%s/v1/providers/%s/%s/%s/download/%s/%s",
		baseURL, ns, name, version, runtime.GOOS, runtime.GOARCH)
	dResp, err := httpGet(ctx, downloadURL)
	if err != nil {
		return "", err
	}
	var meta downloadResponse
	decErr = json.NewDecoder(dResp.Body).Decode(&meta)
	_ = dResp.Body.Close()
	if decErr != nil {
		return "", fmt.Errorf("registry: decoding %s: %w", downloadURL, decErr)
	}

	// 3. Download the zip to a temp file, hashing while streaming.
	zipResp, err := httpGet(ctx, meta.DownloadURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = zipResp.Body.Close() }()

	tmpFile, err := os.CreateTemp("", "tchori-provider-*.zip")
	if err != nil {
		return "", fmt.Errorf("registry: creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }() // best-effort temp file cleanup

	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher), zipResp.Body) //nolint:gosec // size is bounded by the registry's own archive size; content is checksum-verified below before extraction
	closeErr := tmpFile.Close()
	if copyErr != nil {
		return "", fmt.Errorf("registry: downloading %s: %w", meta.DownloadURL, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("registry: writing %s: %w", tmpPath, closeErr)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))

	// 4. Fetch SHA256SUMS and verify our computed hash against the entry
	// for this zip's filename.
	sResp, err := httpGet(ctx, meta.ShasumsURL)
	if err != nil {
		return "", err
	}
	sumsBody, err := io.ReadAll(sResp.Body)
	_ = sResp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("registry: reading %s: %w", meta.ShasumsURL, err)
	}
	want, err := findSHA256(string(sumsBody), meta.Filename)
	if err != nil {
		return "", err
	}
	if sum != want {
		return "", fmt.Errorf("registry: checksum mismatch for %s: computed %s, SHA256SUMS says %s", meta.Filename, sum, want)
	}

	// 5. Extract the terraform-provider-* binary into the cache layout.
	destDir := filepath.Join(cacheDir, ns, name, version, runtime.GOOS+"_"+runtime.GOARCH)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return "", fmt.Errorf("registry: creating %s: %w", destDir, err)
	}

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return "", fmt.Errorf("registry: opening downloaded zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var destPath string
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "terraform-provider-") {
			continue
		}
		safeName, safeErr := safeZipEntryName(f.Name)
		if safeErr != nil {
			return "", safeErr
		}
		destPath = filepath.Join(destDir, safeName)
		if err := extractZipFile(f, destPath); err != nil {
			return "", err
		}
		break
	}
	if destPath == "" {
		return "", fmt.Errorf("registry: no terraform-provider-* file found inside %s", meta.Filename)
	}
	if err := os.Chmod(destPath, 0o755); err != nil { //nolint:gosec // G302: the extracted file is a plugin binary tchori must exec directly; 0755 (not group/world-writable) is the minimum mode that grants the required executable bit
		return "", fmt.Errorf("registry: chmod %s: %w", destPath, err)
	}

	return destPath, nil
}

// Discover finds the binary for source@version: pluginDir (if non-empty)
// is searched first (any file matching terraform-provider-<name>*), then
// the cache layout above.
func Discover(cacheDir, pluginDir, source, version string) (string, error) {
	ns, name, err := parseSource(source)
	if err != nil {
		return "", err
	}
	pattern := "terraform-provider-" + name + "*"

	if pluginDir != "" {
		matches, globErr := filepath.Glob(filepath.Join(pluginDir, pattern))
		if globErr != nil {
			return "", fmt.Errorf("registry: globbing plugin dir %s: %w", pluginDir, globErr)
		}
		if len(matches) > 0 {
			return matches[0], nil
		}
	}

	cacheSubdir := filepath.Join(cacheDir, ns, name, version, runtime.GOOS+"_"+runtime.GOARCH)
	matches, err := filepath.Glob(filepath.Join(cacheSubdir, pattern))
	if err != nil {
		return "", fmt.Errorf("registry: globbing cache dir %s: %w", cacheSubdir, err)
	}
	if len(matches) > 0 {
		return matches[0], nil
	}

	return "", fmt.Errorf("registry: no binary for %s@%s found (searched plugin dir %q and cache dir %s)",
		source, version, pluginDir, cacheSubdir)
}

// Installed describes one provider version found in the cache by List.
type Installed struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

// List walks cacheDir's <namespace>/<name>/<version>/<os_arch>/ layout and
// returns one Installed entry per version directory that actually contains
// a terraform-provider-* binary in some platform subdirectory. A missing
// cacheDir yields an empty, non-error result (nothing installed yet).
func List(cacheDir string) ([]Installed, error) {
	if _, err := os.Stat(cacheDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry: stat %s: %w", cacheDir, err)
	}

	namespaces, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("registry: reading %s: %w", cacheDir, err)
	}

	var out []Installed
	for _, nsEntry := range namespaces {
		if !nsEntry.IsDir() {
			continue
		}
		nsPath := filepath.Join(cacheDir, nsEntry.Name())
		names, err := os.ReadDir(nsPath)
		if err != nil {
			return nil, fmt.Errorf("registry: reading %s: %w", nsPath, err)
		}
		for _, nameEntry := range names {
			if !nameEntry.IsDir() {
				continue
			}
			namePath := filepath.Join(nsPath, nameEntry.Name())
			versions, err := os.ReadDir(namePath)
			if err != nil {
				return nil, fmt.Errorf("registry: reading %s: %w", namePath, err)
			}
			for _, versionEntry := range versions {
				if !versionEntry.IsDir() {
					continue
				}
				versionPath := filepath.Join(namePath, versionEntry.Name())
				matches, globErr := filepath.Glob(filepath.Join(versionPath, "*", "terraform-provider-*"))
				if globErr != nil {
					return nil, fmt.Errorf("registry: globbing %s: %w", versionPath, globErr)
				}
				if len(matches) == 0 {
					continue
				}
				out = append(out, Installed{
					Source:  nsEntry.Name() + "/" + nameEntry.Name(),
					Version: versionEntry.Name(),
					Path:    matches[0],
				})
			}
		}
	}
	return out, nil
}
