//go:build e2e

package e2e

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// newFixtureRegistry serves opentofu/null at nullVersion for the running
// platform. Its archive contains the built protocol-5-only test provider, so
// the install and launch-failure subtests exercise the complete CLI path.
func newFixtureRegistry(t *testing.T, providerBinary string) *httptest.Server {
	t.Helper()

	binary, err := os.ReadFile(providerBinary) //nolint:gosec // fixed test build output inside t.TempDir
	if err != nil {
		t.Fatalf("reading protocol-5 fixture provider: %v", err)
	}

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("terraform-provider-null")
	if err != nil {
		t.Fatalf("creating fixture archive entry: %v", err)
	}
	if _, err := entry.Write(binary); err != nil {
		t.Fatalf("writing fixture archive entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing fixture archive: %v", err)
	}

	archiveBytes := archive.Bytes()
	sum := sha256.Sum256(archiveBytes)
	sumHex := hex.EncodeToString(sum[:])
	filename := fmt.Sprintf("terraform-provider-null_%s_%s_%s.zip", nullVersion, runtime.GOOS, runtime.GOARCH)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/{namespace}/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("namespace") != "opentofu" || r.PathValue("name") != "null" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{{"version": nullVersion, "protocols": []string{"5.0"}}},
		})
	})
	mux.HandleFunc("GET /v1/providers/{namespace}/{name}/{version}/download/{goos}/{goarch}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("namespace") != "opentofu" || r.PathValue("name") != "null" ||
			r.PathValue("version") != nullVersion || r.PathValue("goos") != runtime.GOOS ||
			r.PathValue("goarch") != runtime.GOARCH {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"filename":     filename,
			"download_url": srv.URL + "/dl/" + filename,
			"shasums_url":  srv.URL + "/dl/SHA256SUMS",
			"shasum":       sumHex,
		})
	})
	mux.HandleFunc("GET /dl/"+filename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archiveBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", sumHex, filename)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fixtureProviderPath(binDir string) string {
	return filepath.Join(binDir, "terraform-provider-null-protocol5")
}
