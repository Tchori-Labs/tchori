// Package docs checks repository documentation invariants.
//
// Link detection is deliberately limited to Markdown inline-link syntax. Bare
// paths and inline-code mentions are prose and are outside this package's
// scope.
package docs

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

var inlineLinkPattern = regexp.MustCompile(`\[[^\]\n]*\]\(([^)\s]+)(?:\s+[^)]*)?\)`)

// DanglingLinks returns Markdown links whose repository targets do not exist.
// File names and exists entries must be slash-separated, repo-relative paths.
func DanglingLinks(files map[string][]byte, exists map[string]struct{}) []string {
	var findings []string
	for file, contents := range files {
		matches := inlineLinkPattern.FindAllSubmatch(contents, -1)
		for _, match := range matches {
			target := strings.Trim(string(match[1]), "<>")
			if ignoredTarget(target) {
				continue
			}

			target = strings.SplitN(target, "#", 2)[0]
			if !strings.HasSuffix(strings.ToLower(target), ".md") {
				continue
			}
			if targetExists(file, target, exists) {
				continue
			}
			findings = append(findings, file+": "+string(match[1]))
		}
	}
	sort.Strings(findings)
	return findings
}

func ignoredTarget(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(target, "#") ||
		strings.HasPrefix(target, "//") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "mailto:")
}

func targetExists(file, target string, exists map[string]struct{}) bool {
	rootRelative := path.Clean(strings.TrimPrefix(target, "/"))
	fileRelative := path.Clean(path.Join(path.Dir(file), target))

	_, rootFound := exists[rootRelative]
	_, fileFound := exists[fileRelative]
	return rootFound || fileFound
}
