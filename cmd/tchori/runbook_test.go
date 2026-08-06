package main_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type commandTree struct {
	children map[string]*commandTree
	min      int
	max      int // math.MaxInt means unbounded.
	leaf     bool
}

type fencedBlock struct {
	info       string
	content    string
	preceding  string
	counter    bool
	executable bool
}

var counterexampleMarker = regexp.MustCompile(`^<!-- runbook:counterexample: (.+) -->$`)

func TestCloudflareImportRunbook(t *testing.T) {
	tree := discoverCommandTree(t)
	assertCommandTree(t, tree)

	path := filepath.Join("..", "..", "docs", "acceptance-cloudflare-import.md")
	contentBytes, err := os.ReadFile(path) //nolint:gosec // repository-relative test fixture path
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	if errs := lintRunbook(content, tree); len(errs) != 0 {
		t.Fatalf("runbook lint failed:\n%s", joinErrors(errs))
	}

	mustFail := []struct {
		name, line, contains string
	}{
		{"unknown nested command", "tchori providers schema", "unknown command path"},
		{"mcp tool as operand", "tchori mcp provider_schema", "unexpected operand"},
		{"missing import operands", "tchori import", "missing operand"},
		{"missing state show operand", "tchori state show", "missing operand"},
		{"comment convention", `# always invoke "$TCHORI_BIN" here`, "invocation token inside shell comment"},
		{"partial import operands", "tchori import cloudflare_dns_record.x", "missing operand"},
		{"unknown root command", "tchori adopt", "unknown command path"},
		{"bare group", "tchori providers", "incomplete command path"},
	}
	for _, tc := range mustFail {
		t.Run("mutation/"+tc.name, func(t *testing.T) {
			errs := lintRunbook(insertFirstExecutable(content, tc.line), tree)
			assertErrorContains(t, errs, tc.contains)
			if tc.name == "comment convention" {
				for _, err := range errs {
					if strings.Contains(err.Error(), "command path") {
						t.Fatalf("comment mutation produced path error: %v", err)
					}
				}
			}
		})
	}

	t.Run("mutation/unbraced token", func(t *testing.T) {
		mutated := strings.Replace(content, "${CLOUDFLARE_API_TOKEN}", "$CLOUDFLARE_API_TOKEN", 1)
		assertErrorContains(t, lintRunbook(mutated, tree), "unbraced CLOUDFLARE_API_TOKEN")
	})
	t.Run("mutation/authorization argv", func(t *testing.T) {
		mutated := insertFirstExecutable(content, `curl -H "Authorization: Bearer ${CLOUDFLARE_API_TOKEN}" https://example.invalid`)
		assertErrorContains(t, lintRunbook(mutated, tree), "Authorization header in argv")
	})
	t.Run("mutation/missing heading", func(t *testing.T) {
		mutated := strings.Replace(content, "## Run record", "## Evidence table", 1)
		assertErrorContains(t, lintRunbook(mutated, tree), "missing heading")
	})

	mustPass := map[string]string{
		"unknown command in prose":        content + "\nProse says tchori providers schema is not real.\n",
		"unknown command in marked block": content + "\n<!-- runbook:counterexample: intentionally invalid -->\n```bash\ntchori providers schema\n```\n",
		"mcp operand in prose":            content + "\nNever write tchori mcp provider_schema.\n",
		"tool name in prose":              content + "\nThe provider_schema tool is served over stdio.\n",
		"help minimum exception":          insertFirstExecutable(content, "tchori import --help\ntchori state show --help"),
		"ordinary trailing comment":       strings.Replace(content, "tchori validate`", "tchori validate`", 1),
		"standalone ordinary comment":     insertFirstExecutable(content, "# build from a pinned develop SHA before starting"),
	}
	// Add a comment to a real executable invocation rather than changing inline prose.
	mustPass["trailing shell comment"] = strings.Replace(content, "tchori mcp \\\n", "tchori mcp \\ # then confirm the plan is empty\n", 1)
	for name, mutated := range mustPass {
		t.Run("negative/"+name, func(t *testing.T) {
			if errs := lintRunbook(mutated, tree); len(errs) != 0 {
				t.Fatalf("negative control failed:\n%s", joinErrors(errs))
			}
		})
	}
}

func TestParseOperandBounds(t *testing.T) {
	tests := []struct {
		usage   string
		path    int
		min     int
		max     int
		wantErr bool
	}{
		{"tchori import ADDRESS ID [flags]", 1, 2, 2, false},
		{"tchori state show ADDRESS [flags]", 2, 1, 1, false},
		{"tchori mcp [flags]", 1, 0, 0, false},
		{"tchori foo NAME [EXTRA] [flags]", 1, 1, 2, false},
		{"tchori foo [NAME...] [flags]", 1, 0, math.MaxInt, false},
		{"tchori foo NAME... [flags]", 1, 1, math.MaxInt, false},
		{"", 1, 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.usage, func(t *testing.T) {
			min, max, err := parseOperandBounds(tc.usage, tc.path)
			if min != tc.min || max != tc.max || (err != nil) != tc.wantErr {
				t.Fatalf("got (%d,%d,%v), want (%d,%d,err=%v)", min, max, err, tc.min, tc.max, tc.wantErr)
			}
		})
	}
}

func TestStripShellComment(t *testing.T) {
	tests := map[string]string{
		"tchori plan   # confirm no changes":                                 "tchori plan   ",
		`# Discipline B: always use "$TCHORI_BIN"`:                           "",
		`git -C "$TCHORI_SRC" rev-parse HEAD       # must equal $PINNED_SHA`: `git -C "$TCHORI_SRC" rev-parse HEAD       `,
		`tchori state show "rec#1"`:                                          `tchori state show "rec#1"`,
		"tchori mcp provider_schema # still wrong":                           "tchori mcp provider_schema ",
		"https://example.com/docs#anchor":                                    "https://example.com/docs#anchor",
		"foo#bar":                                                            "foo#bar",
	}
	for input, want := range tests {
		if got := stripShellComment(input); got != want {
			t.Errorf("stripShellComment(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRunbookInvocationExtraction(t *testing.T) {
	tree := discoverCommandTree(t)
	buildSnippet := `# Build from the pinned develop commit in a clean checkout
TCHORI_SRC=/path/to/tchori
PINNED_SHA=f7c28232ee472aeb346de75bd123bc47467e6f53
git -C "$TCHORI_SRC" status --porcelain # must print nothing
mkdir -p "$HOME/.local/tchori-bin"
(cd "$TCHORI_SRC" && go build -o "$HOME/.local/tchori-bin/tchori" ./cmd/tchori)
export PATH="$HOME/.local/tchori-bin:$PATH"
export TCHORI_BIN="$HOME/.local/tchori-bin/tchori"`
	pipeline := `{
  printf '%s\n' '{"name":"provider_schema"}'
} | tchori mcp \
  | jq -r '.result.content[0].text'`
	ok := []string{
		"tchori validate", "tchori plan", "tchori state show cloudflare_dns_record.x",
		"tchori import cloudflare_dns_record.x abc123", "tchori import --help",
		"tchori state show --help", "tchori mcp --help", "tchori --chdir infra/cloudflare plan",
		`"$TCHORI_BIN" version`, "tchori mcp", "tchori providers install cloudflare/cloudflare 5.0.0",
		`printf '%s' '{"name":"provider_schema"}'`, pipeline,
		"tchori state list | grep provider_schema", buildSnippet,
	}
	for _, snippet := range ok {
		t.Run("ok/"+snippet, func(t *testing.T) {
			if errs := lintSnippet(snippet, tree); len(errs) != 0 {
				t.Fatalf("unexpected errors:\n%s", joinErrors(errs))
			}
		})
	}

	errors := []struct{ snippet, contains string }{
		{"tchori mcp provider_schema", "unexpected operand"},
		{"tchori mcp state_show", "unexpected operand"},
		{"tchori mcp provider_schema --help", "unexpected operand"},
		{"tchori version extra", "unexpected operand"},
		{"tchori import", "missing operand"},
		{"tchori import cloudflare_dns_record.x", "missing operand"},
		{"tchori state show", "missing operand"},
		{"tchori apply", "missing operand"},
		{"tchori providers install cloudflare/cloudflare", "missing operand"},
		{"tchori providers schema", "unknown command path"},
		{"tchori providers", "incomplete command path"},
		{"tchori state", "incomplete command path"},
		{"tchori adopt", "unknown command path"},
	}
	for _, tc := range errors {
		t.Run("error/"+tc.snippet, func(t *testing.T) {
			assertErrorContains(t, lintSnippet(tc.snippet, tree), tc.contains)
		})
	}
}

func discoverCommandTree(t *testing.T) *commandTree {
	t.Helper()
	root := &commandTree{children: map[string]*commandTree{}}
	var visit func([]string, *commandTree)
	visit = func(path []string, node *commandTree) {
		args := append(append([]string{}, path...), "--help")
		stdout, stderr, code := runCLI(t, t.TempDir(), args...)
		if code != 0 {
			t.Fatalf("tchori %s --help: exit %d: %s", strings.Join(path, " "), code, stderr)
		}
		children := parseAvailableCommands(stdout)
		if len(children) == 0 {
			node.leaf = true
			usage := parseUsage(stdout)
			min, max, err := parseOperandBounds(usage, len(path))
			if err != nil {
				t.Fatalf("parse usage for %q: %v\n%s", strings.Join(path, " "), err, stdout)
			}
			node.min, node.max = min, max
			return
		}
		for _, child := range children {
			next := &commandTree{children: map[string]*commandTree{}}
			node.children[child] = next
			visit(append(path, child), next)
		}
	}
	visit(nil, root)
	return root
}

func parseAvailableCommands(help string) []string {
	lines := strings.Split(help, "\n")
	inside := false
	var result []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "Available Commands:" {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasSuffix(strings.TrimSpace(line), "Flags:") {
			break
		}
		if !strings.HasPrefix(line, "  ") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 0 {
			// Cobra injects help/completion command families whose generated,
			// variadic usages are not authored in commands.go. The runbook
			// contract covers tchori's product command tree, so omit those
			// framework conveniences while recursively discovering every
			// authored child.
			if fields[0] == "help" || fields[0] == "completion" {
				continue
			}
			result = append(result, fields[0])
		}
	}
	return result
}

func parseUsage(help string) string {
	lines := strings.Split(help, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "Usage:" && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

func parseOperandBounds(usageLine string, pathLen int) (int, int, error) {
	fields := strings.Fields(strings.TrimSpace(usageLine))
	prefix := 1 + pathLen // executable plus command path.
	if len(fields) < prefix || len(fields) == 0 || fields[0] != "tchori" {
		return 0, 0, fmt.Errorf("unparseable Usage line %q", usageLine)
	}
	min, max := 0, 0
	for _, token := range fields[prefix:] {
		if token == "[flags]" || strings.HasPrefix(token, "-") {
			continue
		}
		optional := strings.HasPrefix(token, "[") && strings.HasSuffix(token, "]")
		plain := strings.Trim(token, "[]")
		variadic := strings.HasSuffix(plain, "...")
		if !optional {
			min++
		}
		if variadic {
			max = math.MaxInt
		} else if max != math.MaxInt {
			max++
		}
	}
	return min, max, nil
}

func assertCommandTree(t *testing.T, tree *commandTree) {
	t.Helper()
	groups := []string{"state", "providers"}
	leaves := map[string][2]int{
		"validate": {0, 0}, "plan": {0, 0}, "apply": {1, 1}, "destroy": {0, 0},
		"import": {2, 2}, "state list": {0, 0}, "state show": {1, 1},
		"providers install": {2, 2}, "providers list": {0, 0}, "mcp": {0, 0}, "version": {0, 0},
	}
	for _, path := range groups {
		node := treeAt(tree, path)
		if node == nil || node.leaf {
			t.Errorf("%s is not a discovered group", path)
		}
	}
	for path, bounds := range leaves {
		node := treeAt(tree, path)
		if node == nil || !node.leaf {
			t.Errorf("%s is not a discovered leaf", path)
			continue
		}
		if node.min != bounds[0] || node.max != bounds[1] {
			t.Errorf("%s bounds = (%d,%d), want %v", path, node.min, node.max, bounds)
		}
	}
	if treeAt(tree, "providers schema") != nil {
		t.Error("providers schema unexpectedly exists")
	}
	var walk func(string, *commandTree)
	walk = func(path string, node *commandTree) {
		if node.leaf && node.min != node.max {
			t.Errorf("current leaf %s has optional operands (%d,%d); update invariant deliberately", path, node.min, node.max)
		}
		for name, child := range node.children {
			walk(strings.TrimSpace(path+" "+name), child)
		}
	}
	walk("", tree)
}

func treeAt(root *commandTree, path string) *commandTree {
	node := root
	for _, token := range strings.Fields(path) {
		node = node.children[token]
		if node == nil {
			return nil
		}
	}
	return node
}

func parseFences(content string) ([]fencedBlock, []error) {
	lines := strings.Split(content, "\n")
	var blocks []fencedBlock
	var errs []error
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		info := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		start := i + 1
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
		}
		if i == len(lines) {
			errs = append(errs, fmt.Errorf("unterminated fence at line %d", start))
			break
		}
		preceding := ""
		for p := start - 2; p >= 0; p-- {
			if strings.TrimSpace(lines[p]) != "" {
				preceding = strings.TrimSpace(lines[p])
				break
			}
		}
		counter := false
		if strings.HasPrefix(preceding, "<!-- runbook:counterexample:") {
			match := counterexampleMarker.FindStringSubmatch(preceding)
			if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
				errs = append(errs, fmt.Errorf("counterexample marker requires a reason"))
			} else {
				counter = true
			}
		}
		shell := map[string]bool{"bash": true, "sh": true, "shell": true, "console": true}[info]
		blocks = append(blocks, fencedBlock{info: info, content: strings.Join(lines[start:i], "\n"), preceding: preceding, counter: counter, executable: shell && !counter})
	}
	return blocks, errs
}

// stripShellComment prevents annotations from being mistaken for invocations.
// The document independently forbids invocation tokens in fenced comments;
// both guards are intentional so dropping either one is detected.
func stripShellComment(line string) string {
	single, double := false, false
	for i, r := range line {
		switch r {
		case '\'':
			if !double {
				single = !single
			}
		case '"':
			if !single && (i == 0 || line[i-1] != '\\') {
				double = !double
			}
		case '#':
			if !single && !double && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return line[:i]
			}
		}
	}
	return line
}

func lintRunbook(content string, tree *commandTree) []error {
	blocks, errs := parseFences(content)
	executable := 0
	for _, block := range blocks {
		if block.executable {
			executable++
			errs = append(errs, lintCommentConvention(block.content)...)
			errs = append(errs, lintSnippet(block.content, tree)...)
		}
	}
	if executable == 0 {
		errs = append(errs, fmt.Errorf("no executable snippets found"))
	}
	for _, heading := range []string{"## Prerequisites", "## Checklist", "## Run record"} {
		if !strings.Contains(content, heading) {
			errs = append(errs, fmt.Errorf("missing heading %s", heading))
		}
	}
	if regexp.MustCompile(`\$CLOUDFLARE_API_TOKEN\b`).MatchString(content) {
		errs = append(errs, fmt.Errorf("unbraced CLOUDFLARE_API_TOKEN expansion"))
	}
	if regexp.MustCompile(`\$CLOUDFLARE_ZONE_ID\b`).MatchString(content) {
		errs = append(errs, fmt.Errorf("unbraced CLOUDFLARE_ZONE_ID expansion"))
	}
	configStdin := false
	for _, block := range blocks {
		if strings.Contains(block.content, "--config -") {
			configStdin = true
		}
		for _, line := range strings.Split(block.content, "\n") {
			if strings.Contains(line, "Authorization") && regexp.MustCompile(`(^|\s)(-H|--header)(=|\s)`).MatchString(line) {
				errs = append(errs, fmt.Errorf("Authorization header in argv"))
			}
			if strings.Contains(line, "Authorization: Bearer") && !strings.HasPrefix(strings.TrimSpace(line), `header = "Authorization: Bearer`) {
				errs = append(errs, fmt.Errorf("bearer header outside curl stdin config line"))
			}
		}
	}
	if !configStdin {
		errs = append(errs, fmt.Errorf("no --config - invocation found"))
	}
	return errs
}

func lintCommentConvention(content string) []error {
	var errs []error
	for lineNo, line := range strings.Split(content, "\n") {
		stripped := stripShellComment(line)
		if len(stripped) == len(line) {
			continue
		}
		comment := line[len(stripped):]
		for _, token := range []string{"tchori", `"$TCHORI_BIN"`, "${TCHORI_BIN}", "$TCHORI_BIN"} {
			if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(token) + `([^A-Za-z0-9_]|$)`).MatchString(comment) {
				errs = append(errs, fmt.Errorf("line %d: invocation token inside shell comment: %s", lineNo+1, token))
			}
		}
	}
	return errs
}

func lintSnippet(content string, tree *commandTree) []error {
	lines := strings.Split(content, "\n")
	var logical []string
	current := ""
	for _, raw := range lines {
		line := stripShellComment(raw)
		trimmed := strings.TrimSpace(line)
		continued := strings.HasSuffix(trimmed, "\\")
		if continued {
			trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "\\"))
		}
		if current != "" {
			current += " "
		}
		current += trimmed
		if !continued {
			logical = append(logical, current)
			current = ""
		}
	}
	if current != "" {
		logical = append(logical, current)
	}

	var errs []error
	for _, line := range logical {
		tokens := strings.Fields(line)
		for i, token := range tokens {
			if !isInvocationToken(token) {
				continue
			}
			errs = append(errs, lintInvocation(tokens[i+1:], tree)...)
		}
	}
	return errs
}

func isInvocationToken(token string) bool {
	return token == "tchori" || token == `"$TCHORI_BIN"` || token == "${TCHORI_BIN}" || token == "$TCHORI_BIN" //nolint:gosec // command variable names, not credentials
}

func lintInvocation(tokens []string, root *commandTree) []error {
	node := root
	path := []string{"tchori"}
	help := false
	operands := 0
	var firstExtra string
	for i := 0; i < len(tokens); i++ {
		token := strings.TrimSpace(tokens[i])
		if token == "\\" {
			continue
		}
		if isShellOperator(token) {
			break
		}
		if token == "--help" || token == "-h" {
			help = true
			continue
		}
		if strings.HasPrefix(token, "-") {
			if (token == "--chdir" || token == "--plugin-dir") && i+1 < len(tokens) { //nolint:gosec // CLI flag names, not credentials
				i++
			}
			continue
		}
		if !node.leaf {
			child := node.children[token]
			if child == nil {
				return []error{fmt.Errorf("unknown command path: %s %s", strings.Join(path, " "), token)}
			}
			node = child
			path = append(path, token)
			continue
		}
		operands++
		if firstExtra == "" && node.max != math.MaxInt && operands > node.max {
			firstExtra = token
		}
	}
	if !node.leaf {
		return []error{fmt.Errorf("incomplete command path: %s", strings.Join(path, " "))}
	}
	if firstExtra != "" {
		return []error{fmt.Errorf("unexpected operand: %s %s", strings.Join(path, " "), firstExtra)}
	}
	// Cobra's help path bypasses Args validation, so help relaxes only the
	// minimum. The maximum stays strict to reject `mcp TOOL --help` drift.
	if operands < node.min && !help {
		return []error{fmt.Errorf("missing operand: %s needs at least %d operand(s), found %d", strings.Join(path, " "), node.min, operands)}
	}
	return nil
}

func isShellOperator(token string) bool {
	if strings.HasPrefix(token, "<<") {
		return true
	}
	switch token {
	case "|", "||", "&&", ";", ">", ">>", "<", ")":
		return true
	default:
		return false
	}
}

func insertFirstExecutable(content, line string) string {
	for _, fence := range []string{"```bash\n", "```sh\n", "```shell\n", "```console\n"} {
		if idx := strings.Index(content, fence); idx >= 0 {
			at := idx + len(fence)
			return content[:at] + line + "\n" + content[at:]
		}
	}
	return content
}

func assertErrorContains(t *testing.T, errs []error, want string) {
	t.Helper()
	for _, err := range errs {
		if strings.Contains(err.Error(), want) {
			return
		}
	}
	t.Fatalf("errors did not contain %q:\n%s", want, joinErrors(errs))
}

func joinErrors(errs []error) string {
	parts := make([]string, len(errs))
	for i, err := range errs {
		parts[i] = err.Error()
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}
