package security

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type scriptCase struct {
	name, pvr, state, decision, doc string
	noGH, fatal                     bool
	wantExitZero                    bool
	want                            [4]string
	forbiddenPath                   string
}

func TestVerifySecurityContactScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash genuinely unavailable; security audit harness requires bash")
	}
	good := "Private security contact: https://intake.example.invalid"
	validFallback := fallbackDoc(good, "")
	noRegion := strings.ReplaceAll(strings.ReplaceAll(validFallback, regionStart+"\n"+good+"\n", ""), regionEnd+"\n", "")
	outside := strings.ReplaceAll(validFallback, regionStart+"\n"+good+"\n"+regionEnd, regionStart+"\n"+regionEnd+"\n"+good)
	dual := pvrDoc + strings.TrimPrefix(validFallback, "<!-- security-channel: fallback-contact decision="+fallbackRef+" -->\n")
	dual = "<!-- security-channel: fallback-contact decision=" + fallbackRef + " -->\n" + dual
	placeholderRef := "Tchori-Labs/main:decisions/EXAMPLE-security-contact.md"
	placeholderDoc := strings.ReplaceAll(validFallback, fallbackRef, placeholderRef)
	traversalRef := "Tchori-Labs/main:decisions/../README.md"
	traversalDoc := strings.ReplaceAll(validFallback, fallbackRef, traversalRef)
	fingerprint := strings.Repeat("DEADBEEF", 5)
	pgp := "-----BEGIN PGP PUBLIC KEY BLOCK-----\nNOT-A-REAL-KEY\n-----END PGP PUBLIC KEY BLOCK-----\n"
	cases := []scriptCase{
		{"01 enabled pvr", "enabled", "ok", "ok", pvrDoc, false, false, true, [4]string{"PASS", "PASS", "PASS", "NA"}, ""},
		{"02 disabled pending", "disabled", "ok", "ok", pendingDoc, false, false, false, [4]string{"FAIL", "PASS", "PASS", "NA"}, ""},
		{"03 fallback resolves", "disabled", "ok", "ok", validFallback, false, false, true, [4]string{"PASS", "PASS", "PASS", "PASS"}, ""},
		{"04 decision missing", "disabled", "ok", "404", validFallback, false, false, false, [4]string{"PASS", "PASS", "PASS", "FAIL"}, ""},
		{"05 state 404", "disabled", "404", "ok", validFallback, false, false, false, [4]string{"PASS", "PASS", "PASS", "UA"}, ""},
		{"06 state 403", "disabled", "403", "ok", validFallback, false, false, false, [4]string{"PASS", "PASS", "PASS", "UA"}, ""},
		{"07 pvr error pending", "error", "ok", "ok", pendingDoc, false, false, false, [4]string{"UA", "PASS", "UA", "NA"}, ""},
		{"08 pvr error fallback", "error", "ok", "ok", validFallback, false, false, false, [4]string{"PASS", "PASS", "UA", "PASS"}, ""},
		{"09 enabled pending drift", "enabled", "ok", "ok", pendingDoc, false, false, false, [4]string{"PASS", "PASS", "FAIL", "NA"}, ""},
		{"10 disabled pvr drift", "disabled", "ok", "ok", pvrDoc, false, false, false, [4]string{"FAIL", "PASS", "FAIL", "NA"}, ""},
		{"11 mixed state", "enabled", "ok", "ok", pendingDoc + pvrMarker + "\n", false, false, false, [4]string{"PASS", "FAIL", "PASS", "NA"}, ""},
		{"12 marker free", "disabled", "ok", "ok", "plain policy\n", false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, ""},
		{"13 placeholder ref", "disabled", "ok", "ok", placeholderDoc, false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, "EXAMPLE-security-contact.md"},
		{"14 no gh", "", "", "", pendingDoc, true, false, false, [4]string{"UA", "PASS", "UA", "NA"}, ""},
		{"15 fallback unusable", "disabled", "ok", "ok", noRegion, false, false, false, [4]string{"FAIL", "FAIL", "PASS", "PASS"}, ""},
		{"16 pvr error unusable", "error", "ok", "ok", noRegion, false, false, false, [4]string{"UA", "FAIL", "UA", "PASS"}, ""},
		{"17 traversal ref", "disabled", "ok", "ok", traversalDoc, false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, "../README.md"},
		{"18 missing policy", "disabled", "ok", "ok", "", false, true, false, [4]string{}, ""},
		{"19 dual operative", "enabled", "ok", "ok", dual, false, false, true, [4]string{"PASS", "PASS", "PASS", "PASS"}, ""},
		{"20 duplicate pending", "disabled", "ok", "ok", pendingDoc + pendingMarker + "\n", false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, ""},
		{"21 duplicate pvr", "enabled", "ok", "ok", pvrDoc + pvrMarker + "\n", false, false, false, [4]string{"PASS", "FAIL", "PASS", "NA"}, ""},
		{"22 affordance outside", "disabled", "ok", "ok", outside, false, false, false, [4]string{"FAIL", "FAIL", "PASS", "PASS"}, ""},
		{"23 orphan region", "disabled", "ok", "ok", pendingDoc + regionStart + "\n" + regionEnd + "\n", false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, ""},
		{"24 pgp pending", "disabled", "ok", "ok", pendingDoc + pgp, false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, ""},
		{"25 fingerprint outside", "disabled", "ok", "ok", fallbackDoc(good, fingerprint), false, false, false, [4]string{"FAIL", "FAIL", "PASS", "PASS"}, ""},
		{"26 placeholder pending", "disabled", "ok", "ok", pendingDoc + "Private security contact: <value>\n", false, false, false, [4]string{"FAIL", "FAIL", "PASS", "NA"}, ""},
	}
	root := repositoryRoot(t)
	script := filepath.Join(root, "scripts", "verify-security-contact.sh")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScriptCase(t, bash, script, tc) })
	}
}

func runScriptCase(t *testing.T, bash, script string, tc scriptCase) {
	t.Helper()
	dir := t.TempDir()
	policy := filepath.Join(dir, "SECURITY.md")
	if !tc.fatal {
		if err := os.WriteFile(policy, []byte(tc.doc), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		policy = filepath.Join(dir, "missing.md")
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "gh.log")
	if tc.noGH {
		installUtilityLinks(t, bin)
	} else {
		writeGHStub(t, bin)
	}
	cmd := exec.Command(bash, script) //nolint:gosec // fixed repository script under a discovered bash.
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SECURITY_MD="+policy, "GH_REPO=test/repo", "STATE_REPO=test/state",
		"STUB_PVR="+tc.pvr, "STUB_STATE="+tc.state, "STUB_DECISION="+tc.decision, "STUB_LOG="+logPath,
	)
	if tc.noGH {
		cmd.Env = replaceEnv(cmd.Env, "PATH", bin)
	}
	output, err := cmd.CombinedOutput()
	if tc.wantExitZero && err != nil {
		t.Fatalf("unexpected failure: %v\n%s", err, output)
	}
	if !tc.wantExitZero && err == nil {
		t.Fatalf("unexpected success\n%s", output)
	}
	text := string(output)
	if tc.fatal {
		if strings.Count(text, "C0 —") != 1 {
			t.Fatalf("expected one C0 line: %s", text)
		}
		for i := 1; i <= 4; i++ {
			if strings.Contains(text, fmt.Sprintf("C%d —", i)) {
				t.Fatalf("fatal run emitted C%d: %s", i, text)
			}
		}
		return
	}
	for i, kind := range tc.want {
		id := fmt.Sprintf("C%d —", i+1)
		if strings.Count(text, id) != 1 {
			t.Fatalf("expected exactly one %s line, got:\n%s", id, text)
		}
		var prefix string
		switch kind {
		case "PASS":
			prefix = "PASS: "
		case "FAIL":
			prefix = "FAIL: "
		case "UA":
			prefix = "NOT APPLIED / NOT AUDITABLE: "
		case "NA":
			prefix = "PASS: "
		default:
			t.Fatalf("bad expectation %q", kind)
		}
		line := lineContaining(text, id)
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("%s got %q, want prefix %q", id, line, prefix)
		}
		if kind == "NA" && !strings.Contains(line, "not applicable:") {
			t.Errorf("%s is not explicit not-applicable: %q", id, line)
		}
	}
	//nolint:gosec // G304: logPath is created under this test's TempDir.
	logBytes, _ := os.ReadFile(logPath)
	log := string(logBytes)
	if strings.Contains(log, "application/vnd.github.raw") || strings.Contains(log, ".content") {
		t.Fatalf("script requested decision body: %s", log)
	}
	if tc.forbiddenPath != "" && strings.Contains(log, tc.forbiddenPath) {
		t.Fatalf("script called forbidden path: %s", log)
	}
	if strings.Contains(log, "/contents/") && !strings.Contains(log, "/contents/decisions/") {
		t.Fatalf("script escaped decisions namespace: %s", log)
	}
	if tc.name == "05 state 404" && strings.Contains(lineContaining(text, "C4 —"), "decision file is absent") {
		t.Fatal("repository 404 misreported as missing decision")
	}
	if tc.name == "07 pvr error pending" || tc.name == "14 no gh" || tc.name == "16 pvr error unusable" {
		if !strings.Contains(text, "C2 —") {
			t.Fatal("deferred aggregation omitted C2")
		}
	}
}

func writeGHStub(t *testing.T, bin string) {
	t.Helper()
	stub := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "${STUB_LOG}"
args="$*"
case "$args" in
  *private-vulnerability-reporting*)
    case "${STUB_PVR}" in enabled) printf '{"enabled":true}\n';; disabled) printf '{"enabled":false}\n';; *) echo 'HTTP 503' >&2; exit 1;; esac;;
  "api repos/${STATE_REPO} --jq .full_name")
    case "${STUB_STATE}" in ok) echo "${STATE_REPO}";; 404) echo 'HTTP 404' >&2; exit 1;; *) echo 'HTTP 403' >&2; exit 1;; esac;;
  *"repos/${STATE_REPO}/contents/decisions/"*)
    case "${STUB_DECISION}" in ok) echo 'decisions/record.md';; 404) echo 'HTTP 404' >&2; exit 1;; *) echo 'HTTP 403' >&2; exit 1;; esac;;
  *) echo "unexpected gh invocation: $args" >&2; exit 1;;
esac
`
	path := filepath.Join(bin, "gh")
	if err := os.WriteFile(path, []byte(stub), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G302: the synthetic gh stub must be executable by this test process.
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func installUtilityLinks(t *testing.T, bin string) {
	t.Helper()
	for _, name := range []string{"dirname", "pwd", "mktemp", "rm", "grep", "wc", "tr", "sed", "awk", "cut"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("utility %s unavailable for no-gh case", name)
		}
		if err := os.Symlink(p, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func lineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
