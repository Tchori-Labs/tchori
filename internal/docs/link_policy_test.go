package docs

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDanglingLinks(t *testing.T) {
	exists := map[string]struct{}{
		"AGENTS.md":          {},
		"docs/example.md":    {},
		"docs/releasing.md":  {},
		".github/CODEOWNERS": {},
	}

	tests := []struct {
		name  string
		files map[string][]byte
		want  []string
	}{
		{
			name:  "no links",
			files: map[string][]byte{"docs/example.md": []byte("plain prose")},
		},
		{
			name: "valid link forms",
			files: map[string][]byte{
				"docs/example.md": []byte(strings.Join([]string{
					"[file relative](releasing.md)",
					"[with anchor](releasing.md#consumer-verification)",
					"[anchor only](#section)",
					"[HTTPS](https://example.com/guide.md)",
					"[HTTP](http://example.com/guide.md)",
					"[protocol relative](//example.com/guide.md)",
					"[email](mailto:security@example.com)",
					"[parent](../AGENTS.md)",
					"[non-Markdown](../.github/CODEOWNERS)",
					"[slash root relative](/docs/releasing.md)",
				}, "\n")),
			},
		},
		{
			name: "dangling links are deterministic",
			files: map[string][]byte{
				"docs/zeta.md":    []byte("[missing](z-missing.md)"),
				"docs/example.md": []byte("[missing](does-not-exist.md)"),
			},
			want: []string{
				"docs/example.md: does-not-exist.md",
				"docs/zeta.md: z-missing.md",
			},
		},
		{
			name: "angle-bracket destination with a space is checked",
			files: map[string][]byte{
				"docs/example.md": []byte("[guide](<missing guide.md>)"),
			},
			want: []string{
				"docs/example.md: <missing guide.md>",
			},
		},
		{
			name: "relative target is not resolved against the repo root",
			files: map[string][]byte{
				"docs/example.md": []byte("[broken](docs/releasing.md)"),
			},
			want: []string{
				"docs/example.md: docs/releasing.md",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DanglingLinks(tt.files, exists)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DanglingLinks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNoDanglingDocLinksLiveRepo(t *testing.T) {
	root := repositoryRoot(t)
	files := make(map[string][]byte)
	exists := make(map[string]struct{})

	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}

		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		exists[relative] = struct{}{}

		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		contents, err := os.ReadFile(filePath) //nolint:gosec // G304: test reads fixed in-repo Markdown files.
		if err != nil {
			return err
		}
		files[relative] = contents
		return nil
	})
	if err != nil {
		t.Fatalf("collect live repository documentation: %v", err)
	}

	if findings := DanglingLinks(files, exists); len(findings) != 0 {
		sort.Strings(findings)
		t.Fatalf("dangling Markdown links:\n%s", strings.Join(findings, "\n"))
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect repository root candidate %q: %v", dir, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root containing go.mod")
		}
		dir = parent
	}
}
