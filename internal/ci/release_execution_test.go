package ci

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Run the actual workflow shell, not a second implementation of its policy.
func releaseStep(t *testing.T, job, name string) string {
	t.Helper()
	var doc struct {
		Jobs map[string]struct {
			Steps []struct{ Name, Run string }
		}
	}
	if err := yaml.Unmarshal(readRepositoryFile(t, ".github", "workflows", "release.yml"), &doc); err != nil {
		t.Fatal(err)
	}
	for _, step := range doc.Jobs[job].Steps {
		if step.Name == name {
			return step.Run
		}
	}
	t.Fatalf("missing %s step in %s", name, job)
	return ""
}

func releaseShell(t *testing.T, dir, script string, env ...string) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release jobs execute on Linux")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script) //nolint:gosec // G204: executes only the repository workflow step under test
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GITHUB_WORKSPACE="+root)
	cmd.Env = append(cmd.Env, env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("release shell: %s", output)
	}
	return err
}

const verifyReleaseArtifactMatrix = `bash "$GITHUB_WORKSPACE/scripts/verify-release-artifacts.sh" "$RELEASE_TAG"`

func writeCompleteReleaseArtifactMatrix(t *testing.T, dir string) string {
	t.Helper()
	dist := filepath.Join(dir, "dist")
	if err := os.Mkdir(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := ""
	for _, osName := range []string{"darwin", "linux", "windows"} {
		extension := "tar.gz"
		if osName == "windows" {
			extension = "zip"
		}
		for _, arch := range []string{"amd64", "arm64"} {
			archive := fmt.Sprintf("tchori_0.1.0_%s_%s.%s", osName, arch, extension)
			for name, body := range map[string][]byte{
				archive:                []byte("fixture archive"),
				archive + ".sbom.json": []byte(`{"spdxVersion":"SPDX-2.3"}`),
			} {
				if err := os.WriteFile(filepath.Join(dist, name), body, 0o600); err != nil {
					t.Fatal(err)
				}
				manifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(body), name)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dist
}

func TestReleaseRejectsUnmergedTag(t *testing.T) {
	for _, job := range []string{"dry-run", "publish"} {
		t.Run(job, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed fixture commands in an isolated temporary repository
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
					"GIT_AUTHOR_NAME=Tchorizo", "GIT_AUTHOR_EMAIL=agent@example.invalid", "GIT_COMMITTER_NAME=Tchorizo", "GIT_COMMITTER_EMAIL=agent@example.invalid")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			git("init", "-b", "main")
			git("commit", "--allow-empty", "-m", "test: reviewed commit")
			git("update-ref", "refs/remotes/origin/main", "HEAD")
			git("tag", "v0.1.0")
			script := releaseStep(t, job, "Validate release tag and dispatch ref")
			env := []string{"GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_REF=refs/heads/main", "RELEASE_TAG=v0.1.0"}
			if err := releaseShell(t, dir, script, env...); err != nil {
				t.Fatal("reviewed tag must be accepted")
			}
			git("checkout", "-b", "unreviewed")
			git("commit", "--allow-empty", "-m", "test: unreviewed commit")
			git("tag", "v0.2.0")
			env[2] = "RELEASE_TAG=v0.2.0"
			if err := releaseShell(t, dir, script, env...); err == nil {
				t.Fatal("release accepted a tag outside reviewed main history")
			}
		})
	}
}

func TestReleaseArtifactMatrixRejectsIncompletePlatforms(t *testing.T) {
	dir := t.TempDir()
	dist := filepath.Join(dir, "dist")
	if err := os.Mkdir(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	// The old gate accepted only these three files instead of six
	// platform archives and one companion SBOM for each archive.
	for _, name := range []string{"tchori_0.1.0_linux_amd64.tar.gz", "tchori_0.1.0_windows_amd64.zip", "tchori_0.1.0_linux_amd64.tar.gz.sbom.json", "checksums.txt"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := releaseShell(t, dir, verifyReleaseArtifactMatrix, "RELEASE_TAG=v0.1.0"); err == nil {
		t.Fatal("release accepted an incomplete platform matrix")
	}
}

func TestReleaseArtifactMatrixVerifiesCompleteArtifacts(t *testing.T) {
	dir := t.TempDir()
	dist := writeCompleteReleaseArtifactMatrix(t, dir)
	if err := releaseShell(t, dir, verifyReleaseArtifactMatrix, "RELEASE_TAG=v0.1.0"); err != nil {
		t.Fatal("release rejected complete artifact matrix")
	}
	if err := os.WriteFile(filepath.Join(dist, "tchori_0.1.0_linux_amd64.tar.gz"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := releaseShell(t, dir, verifyReleaseArtifactMatrix, "RELEASE_TAG=v0.1.0"); err == nil {
		t.Fatal("release accepted corrupted archive bytes")
	}
}

func TestReleaseSignatureVerifierFailureStopsAcceptance(t *testing.T) {
	tests := []struct {
		job         string
		workflowRef string
		currentRef  string
	}{
		{
			job:         "dry-run",
			workflowRef: "Tchori-Labs/tchori/.github/workflows/release.yml@refs/heads/main",
			currentRef:  "refs/heads/main",
		},
		{
			job:         "publish",
			workflowRef: "Tchori-Labs/tchori/.github/workflows/release.yml@refs/tags/v0.1.0",
			currentRef:  "refs/tags/v0.1.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.job, func(t *testing.T) {
			dir := t.TempDir()
			dist := writeCompleteReleaseArtifactMatrix(t, dir)
			for name, body := range map[string]string{
				"checksums.txt.sig": "untrusted fixture signature",
				"checksums.txt.pem": "untrusted fixture certificate",
			} {
				if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "cosign"), []byte("#!/usr/bin/env bash\nprintf 'simulated verifier failure\\n' >&2\nexit 42\n"), 0o700); err != nil { //nolint:gosec // owner-only executable test fixture
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			marker := filepath.Join(dir, "accepted")
			script := releaseStep(t, tt.job, "Verify signed release outputs") + "\nprintf 'accepted\\n' > \"$ACCEPTANCE_MARKER\""
			env := []string{
				"RELEASE_TAG=v0.1.0",
				"GITHUB_WORKFLOW_REF=" + tt.workflowRef,
				"GITHUB_REF=" + tt.currentRef,
				"ACCEPTANCE_MARKER=" + marker,
			}
			if err := releaseShell(t, dir, script, env...); err == nil {
				t.Fatal("release accepted artifacts after external signature verifier failure")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("acceptance continued after signature verification failed: %v", err)
			}
		})
	}
}

func TestPublishReleasePreservesStableLatestAcrossPrereleases(t *testing.T) {
	var config struct {
		Release struct {
			Prerelease string `yaml:"prerelease"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(readRepositoryFile(t, ".goreleaser.yml"), &config); err != nil {
		t.Fatal(err)
	}
	if config.Release.Prerelease != "auto" {
		t.Fatalf("release.prerelease = %q, want auto", config.Release.Prerelease)
	}

	script := releaseStep(t, "publish", "Publish attested draft release")
	for _, tc := range []struct {
		tag           string
		wantFlag      string
		forbiddenFlag string
	}{
		{tag: "v1.2.3", wantFlag: "--latest", forbiddenFlag: "--prerelease"},
		{tag: "v1.2.4-rc.1", wantFlag: "--prerelease", forbiddenFlag: "--latest"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			argsPath := filepath.Join(dir, "gh.args")
			fake := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" > \"$GH_ARGS\"\n"
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o700); err != nil { //nolint:gosec // owner-only executable test fixture
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := releaseShell(t, dir, script, "RELEASE_TAG="+tc.tag, "GH_ARGS="+argsPath, "GITHUB_REPOSITORY=Tchori-Labs/tchori"); err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(argsPath) //nolint:gosec // test-controlled fake gh output
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), tc.wantFlag) || strings.Contains(string(args), tc.forbiddenFlag) {
				t.Fatalf("gh args = %q, want %s and no %s", args, tc.wantFlag, tc.forbiddenFlag)
			}
		})
	}
}
