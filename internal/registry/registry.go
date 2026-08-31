// Package registry downloads, verifies, and locates Terraform-protocol
// provider binaries from an OpenTofu-compatible provider registry
// (https://opentofu.org/docs/internals/provider-registry-protocol/).
//
// Provider sources must be lowercase alphanumeric namespace/name identifiers
// with optional internal hyphens, and versions must be exact X.Y.Z strings.
// Unsafe or ambiguous path segments are rejected rather than sanitized.
package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// defaultBaseURL is used by Install when baseURL is empty.
const defaultBaseURL = "https://registry.opentofu.org"

// maxProviderArchiveBytes caps the provider archive downloaded to local
// temporary storage. Real provider archives are at most a few hundred MB, so
// 1 GiB accommodates supported providers while bounding disk use if a registry
// or CDN streams an untrusted oversized response. This local-download DoS guard
// is independent of maxZipEntryBytes, which limits each entry during extraction.
// A var (not const) lets tests exercise the boundary with small fixtures.
var maxProviderArchiveBytes int64 = 1 << 30 // 1 GiB

// maxZipEntryBytes caps how much of a single zip entry Install will copy to
// disk during extraction. Provider binaries are a few tens of MB at most;
// this bound exists purely to give io.Copy an upper limit so a malicious or
// corrupt archive cannot be used to exhaust disk space (G110
// decompression-bomb guard). A var (not const) so tests can lower it to
// exercise the oversized-entry path without generating a real multi-GB file.
var maxZipEntryBytes int64 = 1 << 30 // 1 GiB

var (
	// httpDialTimeout bounds DNS resolution and TCP connection setup for every registry fetch.
	httpDialTimeout = 30 * time.Second
	// httpTLSHandshakeTimeout bounds TLS negotiation after a connection is established.
	httpTLSHandshakeTimeout = 15 * time.Second
	// httpResponseHeaderTimeout bounds the wait for response headers after the request is written.
	httpResponseHeaderTimeout = 30 * time.Second
	// httpMetadataReadTimeout bounds complete metadata requests, including body reads.
	httpMetadataReadTimeout = time.Minute
	// httpArchiveReadTimeout bounds complete provider archive requests.
	httpArchiveReadTimeout = 30 * time.Minute

	// providerIdentifierPattern accepts OpenTofu-compatible namespace and
	// provider-name segments: lowercase alphanumeric groups separated by single
	// hyphens. It retains addresses such as opentofu/random and
	// tchori-labs/metaads while excluding path syntax and ambiguous punctuation.
	providerIdentifierPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

	// providerVersionPattern matches the exact numeric X.Y.Z versions accepted
	// by config validation. Keeping this boundary equally strict protects the
	// CLI install path, which does not pass through config validation.
	providerVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// validateSegment rejects a registry coordinate that could alter either the
// provider registry URL structure or the cache path. Values are never cleaned
// or rewritten: callers must provide one canonical namespace, name, or version
// segment. Namespace and name accept lowercase alphanumeric groups with
// internal hyphens; version accepts an exact numeric X.Y.Z string.
func validateSegment(kind, value string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("registry: invalid %s %q", kind, value)
	}
	if strings.ContainsRune(value, 0) || strings.ContainsRune(value, '/') ||
		strings.ContainsRune(value, '\\') || strings.ContainsRune(value, os.PathSeparator) {
		return fmt.Errorf("registry: invalid %s %q: want one path segment", kind, value)
	}
	if filepath.IsAbs(value) || isWindowsAbsolutePath(value) {
		return fmt.Errorf("registry: invalid %s %q: absolute paths are not allowed", kind, value)
	}
	if filepath.Clean(value) != value {
		return fmt.Errorf("registry: invalid %s %q: want a canonical path segment", kind, value)
	}

	pattern := providerIdentifierPattern
	if kind == "version" {
		pattern = providerVersionPattern
	}
	if !pattern.MatchString(value) {
		return fmt.Errorf("registry: invalid %s %q", kind, value)
	}
	return nil
}

// isWindowsAbsolutePath detects drive-rooted and UNC forms even when tchori is
// running on a non-Windows host, where filepath.IsAbs would not recognize them.
func isWindowsAbsolutePath(value string) bool {
	if strings.HasPrefix(value, `\\`) || strings.HasPrefix(value, `//`) {
		return true
	}
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') ||
		(value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' &&
		(value[2] == '/' || value[2] == '\\')
}

// assertUnderRoot verifies lexical containment after resolving both paths to
// clean absolute forms. It is a belt-and-suspenders check in addition to the
// strict segment allow-lists used to construct provider cache destinations.
func assertUnderRoot(root, dest string) error {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return fmt.Errorf("registry: resolving cache root %q: %w", root, err)
	}
	destAbs, err := filepath.Abs(filepath.Clean(dest))
	if err != nil {
		return fmt.Errorf("registry: resolving cache destination %q: %w", dest, err)
	}
	rel, err := filepath.Rel(rootAbs, destAbs)
	if err != nil {
		return fmt.Errorf("registry: checking cache destination %q: %w", destAbs, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("registry: cache destination %q escapes root %q", destAbs, rootAbs)
	}
	return nil
}

// defaultCacheDir returns ~/.tchori/providers, used by Install when
// cacheDir is empty.
func defaultCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("registry: resolving home directory: %w", err)
	}
	return filepath.Join(home, ".tchori", "providers"), nil
}

// parseSource splits and validates an exact "namespace/name" provider source.
// Both parts must be canonical lowercase alphanumeric identifiers with optional
// internal hyphens; path separators and traversal forms are rejected.
func parseSource(source string) (namespace, name string, err error) {
	parts := strings.Split(source, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("registry: invalid provider source %q, want NAMESPACE/NAME", source)
	}
	if err := validateSegment("namespace", parts[0]); err != nil {
		return "", "", err
	}
	if err := validateSegment("provider name", parts[1]); err != nil {
		return "", "", err
	}
	return parts[0], parts[1], nil
}

// newRegistryClient builds an isolated client and transport from the current timeout policy.
// Per-request deadlines are applied by httpGet so metadata and archives retain distinct bounds.
func newRegistryClient() *http.Client {
	dialer := &net.Dialer{Timeout: httpDialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   httpTLSHandshakeTimeout,
		ResponseHeaderTimeout: httpResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport}
}

// versionsResponse is the shape of GET /v1/providers/{ns}/{name}/versions.
type versionsResponse struct {
	Versions []struct {
		Version string `json:"version"`
	} `json:"versions"`
}

// gpgPublicKey is an allowed provider-checksum signing key advertised by the
// registry. ASCIIArmor is the directly trusted OpenPGP public-key material;
// TrustSignature is parsed for protocol compatibility but is not honored.
type gpgPublicKey struct {
	KeyID          string `json:"key_id"`
	ASCIIArmor     string `json:"ascii_armor"`
	TrustSignature string `json:"trust_signature"`
}

// downloadResponse is the shape of
// GET /v1/providers/{ns}/{name}/{version}/download/{os}/{arch}.
type downloadResponse struct {
	Filename    string `json:"filename"`
	DownloadURL string `json:"download_url"`
	ShasumsURL  string `json:"shasums_url"`
	Shasum      string `json:"shasum"`

	// ShasumsSignatureURL locates the binary detached OpenPGP signature over
	// the exact bytes served by ShasumsURL.
	ShasumsSignatureURL string `json:"shasums_signature_url"`
	// SigningKeys lists the only OpenPGP public keys allowed to authenticate
	// that detached signature. Missing or malformed keys fail closed.
	SigningKeys struct {
		GPGPublicKeys []gpgPublicKey `json:"gpg_public_keys"`
	} `json:"signing_keys"`
}

// cancelReadCloser keeps a request deadline alive until its response body is closed.
type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// httpGet issues a bounded GET request and returns a successful response with
// its body still open. Its derived request context covers response body reads
// and inherits caller cancellation.
func httpGet(ctx context.Context, client *http.Client, url string, readTimeout time.Duration) (*http.Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, readTimeout)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil) //nolint:gosec // url is built from a fixed base + validated path segments, never raw user input
	if err != nil {
		cancel()
		return nil, fmt.Errorf("registry: building request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("registry: GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("registry: GET %s: unexpected status %s", url, resp.Status)
	}
	resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// verifyShasumsSignature authenticates the exact SHA256SUMS bytes that will
// subsequently be parsed. It fails closed unless every advertised key is valid
// OpenPGP material and the binary detached signature verifies against one of
// those directly advertised keys; registry trust_signature delegation is not
// followed.
func verifyShasumsSignature(sumsBytes, sigBytes []byte, keys []gpgPublicKey) error {
	if len(keys) == 0 {
		return fmt.Errorf("registry: no usable signing key advertised for SHA256SUMS")
	}

	allEmpty := true
	for _, key := range keys {
		if strings.TrimSpace(key.ASCIIArmor) != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		return fmt.Errorf("registry: no usable signing key advertised for SHA256SUMS")
	}

	var keyring openpgp.EntityList
	for i, key := range keys {
		if strings.TrimSpace(key.ASCIIArmor) == "" {
			return fmt.Errorf("registry: advertised signing key %d has empty ascii_armor", i)
		}
		entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(key.ASCIIArmor))
		if err != nil {
			return fmt.Errorf("registry: malformed advertised signing key %d: %w", i, err)
		}
		if len(entities) == 0 {
			return fmt.Errorf("registry: malformed advertised signing key %d: no OpenPGP entity", i)
		}
		keyring = append(keyring, entities...)
	}

	if _, err := openpgp.CheckDetachedSignature(
		keyring,
		bytes.NewReader(sumsBytes),
		bytes.NewReader(sigBytes),
		nil,
	); err != nil {
		return fmt.Errorf("registry: SHA256SUMS signature verification failed: %w", err)
	}
	return nil
}

// normalizeSHA256 validates an exact 32-byte hexadecimal digest and returns
// its canonical lowercase representation.
func normalizeSHA256(value, source string) (string, error) {
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("registry: invalid SHA256 for %s: want 64 hexadecimal characters", source)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("registry: invalid SHA256 for %s: %w", source, err)
	}
	return strings.ToLower(value), nil
}

// findSHA256 is the single source of SHA256SUMS filename lookup. It requires
// exactly one case-sensitive filename match, validates that entry's digest,
// and rejects duplicate entries even when their hashes are equal.
func findSHA256(sumsBytes []byte, filename string) (string, error) {
	var match string
	matches := 0
	for _, line := range strings.Split(string(sumsBytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != filename {
			continue
		}
		matches++
		if matches > 1 {
			return "", fmt.Errorf("registry: duplicate SHA256SUMS entries for %s", filename)
		}
		var err error
		match, err = normalizeSHA256(fields[0], "SHA256SUMS entry for "+filename)
		if err != nil {
			return "", err
		}
	}
	if matches == 0 {
		return "", fmt.Errorf("registry: no SHA256SUMS entry for %s", filename)
	}
	return match, nil
}

// extractZipFile securely publishes one zip entry at destPath. A pre-existing
// non-regular destination is rejected using Lstat so symlinks are never
// followed. The entry is copied, with the maxZipEntryBytes decompression-bomb
// bound, to a fresh owner-only temp file in the destination directory, then
// fsynced, chmoded 0755, closed, and atomically renamed into place. Failures
// remove only that owned temp file, never the final destination. Entry names
// have already been validated by the caller to rule out zip-slip (absolute
// paths / ".." traversal) before this is called.
func extractZipFile(f *zip.File, destPath string) error {
	info, err := os.Lstat(destPath)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("registry: refusing to install over non-regular destination %s", destPath)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("registry: inspecting destination %s: %w", destPath, err)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("registry: opening %s in zip: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	// CreateTemp creates the file with mode 0600. Keeping it in destPath's
	// directory guarantees that the final rename stays on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".terraform-provider-*.tmp") //nolint:gosec // CreateTemp uses a fixed pattern to create a unique 0600 file in the cache destination directory; it never opens destPath
	if err != nil {
		return fmt.Errorf("registry: creating temporary provider file in %s: %w", filepath.Dir(destPath), err)
	}
	tmpPath := tmp.Name()
	cleanupTemp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	// Ask for one byte more than the cap. io.CopyN silently stops (with a
	// nil error) once it has copied n bytes even if the source has more to
	// give, so requesting exactly maxZipEntryBytes can never distinguish an
	// oversized entry from one that exactly fits: both would come back with
	// err == nil. Requesting maxZipEntryBytes+1 instead means a nil error
	// (or n > maxZipEntryBytes) only happens when the entry is too big, and
	// an entry within the limit still ends the copy via io.EOF as before.
	n, copyErr := io.CopyN(tmp, rc, maxZipEntryBytes+1)
	if copyErr != nil && copyErr != io.EOF {
		cleanupTemp()
		return fmt.Errorf("registry: writing temporary provider file for %s: %w", destPath, copyErr)
	}
	if copyErr == nil || n > maxZipEntryBytes {
		cleanupTemp()
		return fmt.Errorf("registry: zip entry %q exceeds %d byte limit", f.Name, maxZipEntryBytes)
	}
	// Set the final mode before Sync so the durability barrier covers both the
	// verified contents and executable/non-writable permission metadata.
	if err := tmp.Chmod(0o755); err != nil { //nolint:gosec // G302: provider binaries must be executable; 0755 grants execute without group/world write
		cleanupTemp()
		return fmt.Errorf("registry: chmod temporary provider file for %s: %w", destPath, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanupTemp()
		return fmt.Errorf("registry: syncing temporary provider file for %s: %w", destPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("registry: closing temporary provider file for %s: %w", destPath, err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("registry: publishing provider file %s: %w", destPath, err)
	}
	return nil
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
// authenticates the exact SHA256SUMS bytes with a detached OpenPGP signature
// from an advertised key, cross-checks the archive SHA256 against both the
// signed entry and descriptor, then extracts the provider binary to
// cacheDir/<ns>/<name>/<version>/<os>_<arch>/ and returns its path. cacheDir
// default: ~/.tchori/providers. No cache path is created before verification.
func Install(ctx context.Context, source, version, baseURL, cacheDir string) (string, error) {
	ns, name, err := parseSource(source)
	if err != nil {
		return "", err
	}
	if err := validateSegment("version", version); err != nil {
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
	cacheRoot, err := filepath.Abs(filepath.Clean(cacheDir))
	if err != nil {
		return "", fmt.Errorf("registry: resolving cache root %q: %w", cacheDir, err)
	}

	client := newRegistryClient()
	defer client.CloseIdleConnections()

	// 1. GET versions and validate the requested version is actually offered.
	versionsURL := fmt.Sprintf("%s/v1/providers/%s/%s/versions", baseURL, ns, name)
	vResp, err := httpGet(ctx, client, versionsURL, httpMetadataReadTimeout)
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
	dResp, err := httpGet(ctx, client, downloadURL, httpMetadataReadTimeout)
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
	zipResp, err := httpGet(ctx, client, meta.DownloadURL, httpArchiveReadTimeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = zipResp.Body.Close() }()
	if zipResp.ContentLength > maxProviderArchiveBytes {
		return "", fmt.Errorf("registry: provider archive %s declares %d bytes, exceeds %d byte limit",
			meta.Filename, zipResp.ContentLength, maxProviderArchiveBytes)
	}

	tmpFile, err := os.CreateTemp("", "tchori-provider-*.zip")
	if err != nil {
		return "", fmt.Errorf("registry: creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }() // best-effort temp file cleanup

	hasher := sha256.New()
	// Request one byte beyond the local cap so unknown, chunked, or falsely
	// under-reported Content-Length responses cannot bypass the download bound.
	n, copyErr := io.CopyN(io.MultiWriter(tmpFile, hasher), zipResp.Body, maxProviderArchiveBytes+1)
	closeErr := tmpFile.Close()
	if n > maxProviderArchiveBytes {
		return "", fmt.Errorf("registry: provider archive %s exceeds %d byte limit",
			meta.Filename, maxProviderArchiveBytes)
	}
	if copyErr != nil && copyErr != io.EOF {
		return "", fmt.Errorf("registry: downloading %s: %w", meta.DownloadURL, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("registry: writing %s: %w", tmpPath, closeErr)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))

	// 4. Fetch SHA256SUMS once, authenticate those exact bytes with its
	// detached signature, and only then trust its entry for this zip.
	sResp, err := httpGet(ctx, client, meta.ShasumsURL, httpMetadataReadTimeout)
	if err != nil {
		return "", err
	}
	sumsBody, err := io.ReadAll(sResp.Body)
	_ = sResp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("registry: reading %s: %w", meta.ShasumsURL, err)
	}
	if strings.TrimSpace(meta.ShasumsSignatureURL) == "" {
		return "", fmt.Errorf("registry: missing required shasums_signature_url for %s", meta.Filename)
	}
	sigResp, err := httpGet(ctx, client, meta.ShasumsSignatureURL, httpMetadataReadTimeout)
	if err != nil {
		return "", err
	}
	sigBytes, err := io.ReadAll(sigResp.Body)
	_ = sigResp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("registry: reading %s: %w", meta.ShasumsSignatureURL, err)
	}
	if err := verifyShasumsSignature(sumsBody, sigBytes, meta.SigningKeys.GPGPublicKeys); err != nil {
		return "", err
	}

	entryHash, err := findSHA256(sumsBody, meta.Filename)
	if err != nil {
		return "", err
	}
	descriptorHash, err := normalizeSHA256(meta.Shasum, "download descriptor shasum")
	if err != nil {
		return "", err
	}
	if descriptorHash != entryHash {
		return "", fmt.Errorf("registry: descriptor shasum disagrees with signed SHA256SUMS for %s", meta.Filename)
	}
	if sum != entryHash {
		return "", fmt.Errorf("registry: checksum mismatch for %s: computed %s, SHA256SUMS says %s", meta.Filename, sum, entryHash)
	}

	// 5. Extract the terraform-provider-* binary into the cache layout.
	destDir := filepath.Join(cacheRoot, ns, name, version, runtime.GOOS+"_"+runtime.GOARCH)
	if err := assertUnderRoot(cacheRoot, destDir); err != nil {
		return "", err
	}
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
	if err := validateSegment("version", version); err != nil {
		return "", err
	}
	cacheRoot, err := filepath.Abs(filepath.Clean(cacheDir))
	if err != nil {
		return "", fmt.Errorf("registry: resolving cache root %q: %w", cacheDir, err)
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

	cacheSubdir := filepath.Join(cacheRoot, ns, name, version, runtime.GOOS+"_"+runtime.GOARCH)
	if err := assertUnderRoot(cacheRoot, cacheSubdir); err != nil {
		return "", err
	}
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
// cacheDir yields an empty, non-error result (nothing installed yet). Invalid
// legacy or externally-created coordinate directories are ignored.
func List(cacheDir string) ([]Installed, error) {
	cacheRoot, err := filepath.Abs(filepath.Clean(cacheDir))
	if err != nil {
		return nil, fmt.Errorf("registry: resolving cache root %q: %w", cacheDir, err)
	}
	if _, err := os.Stat(cacheRoot); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("registry: stat %s: %w", cacheRoot, err)
	}

	namespaces, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, fmt.Errorf("registry: reading %s: %w", cacheRoot, err)
	}

	var out []Installed
	for _, nsEntry := range namespaces {
		if !nsEntry.IsDir() || validateSegment("namespace", nsEntry.Name()) != nil {
			continue
		}
		nsPath := filepath.Join(cacheRoot, nsEntry.Name())
		if err := assertUnderRoot(cacheRoot, nsPath); err != nil {
			return nil, err
		}
		names, err := os.ReadDir(nsPath)
		if err != nil {
			return nil, fmt.Errorf("registry: reading %s: %w", nsPath, err)
		}
		for _, nameEntry := range names {
			if !nameEntry.IsDir() || validateSegment("provider name", nameEntry.Name()) != nil {
				continue
			}
			namePath := filepath.Join(nsPath, nameEntry.Name())
			if err := assertUnderRoot(cacheRoot, namePath); err != nil {
				return nil, err
			}
			versions, err := os.ReadDir(namePath)
			if err != nil {
				return nil, fmt.Errorf("registry: reading %s: %w", namePath, err)
			}
			for _, versionEntry := range versions {
				if !versionEntry.IsDir() || validateSegment("version", versionEntry.Name()) != nil {
					continue
				}
				versionPath := filepath.Join(namePath, versionEntry.Name())
				if err := assertUnderRoot(cacheRoot, versionPath); err != nil {
					return nil, err
				}
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
