package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	pendingDoc = `<!-- security-channel: pending -->
See [the runbook](docs/security-disclosure-channel.md).
> **Maintainer action required:** establish a private channel.
`
	pvrDoc = `<!-- security-channel: github-pvr -->
Private vulnerability reporting is enabled and operative.
`
	fallbackRef = "Tchori-Labs/main:decisions/2026-security-channel.md"
)

func fallbackDoc(contact, outside string) string {
	return `<!-- security-channel: fallback-contact decision=` + fallbackRef + ` -->
The board-approved fallback is recorded at ` + fallbackRef + `.
<!-- security-contact:published:start -->
` + contact + `
<!-- security-contact:published:end -->
` + outside
}

func TestLiveDisclosurePolicy(t *testing.T) {
	root := repositoryRoot(t)
	//nolint:gosec // G304: fixed in-repository policy path.
	policy, err := os.ReadFile(filepath.Join(root, "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G304: fixed in-repository README path.
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDisclosureChannelConsistency(policy); err != nil {
		t.Errorf("live SECURITY.md: %v", err)
	}
	if err := CheckNoSyntheticContact(policy); err != nil {
		t.Errorf("live SECURITY.md synthetic guard: %v", err)
	}
	if err := CheckPolicyLinked(readme); err != nil {
		t.Errorf("live README: %v", err)
	}
	markers, _ := ChannelMarkers(policy)
	if markers.Pending == 1 {
		if ContactAffordancePresent(policy) {
			t.Error("pending live policy has a contact affordance")
		}
		if ContactShapedValuePresent(policy) {
			t.Error("pending live policy has a contact-shaped value")
		}
	}
}

func TestContactDetectorContainment(t *testing.T) {
	affordances := []string{
		"mailto:team@example.invalid",
		"team@example.invalid",
		"Private security contact: https://intake.example.invalid",
		"Private security contact: <value>",
		"- Private security contact: <tbd>",
	}
	for _, fixture := range affordances {
		if !ContactAffordancePresent([]byte(fixture)) {
			t.Errorf("affordance not detected: %q", fixture)
		}
		if !ContactShapedValuePresent([]byte(fixture)) {
			t.Errorf("containment violated: %q", fixture)
		}
	}
	widerOnly := []string{
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\nNOT-A-REAL-KEY\n-----END PGP PUBLIC KEY BLOCK-----",
		strings.Repeat("DEADBEEF", 5),
		"0x" + strings.Repeat("ABCD", 4),
	}
	for _, fixture := range widerOnly {
		if !ContactShapedValuePresent([]byte(fixture)) {
			t.Errorf("contact-shaped value not detected")
		}
		if ContactAffordancePresent([]byte(fixture)) {
			t.Errorf("wider-only fixture counted as affordance")
		}
	}
	if ContactAffordancePresent([]byte("a future board-approved private security contact may exist")) {
		t.Error("narrative prose counted as affordance")
	}
}

func TestDisclosureConsistencyFixtures(t *testing.T) {
	fingerprint := strings.Repeat("DEADBEEF", 5)
	longID := "0x" + strings.Repeat("ABCD", 4)
	goodContact := "Private security contact: https://intake.example.invalid"
	both := strings.Replace(pvrDoc, "\n", "\n<!-- security-channel: fallback-contact decision="+fallbackRef+" -->\nThe board-approved fallback is recorded at "+fallbackRef+".\n<!-- security-contact:published:start -->\n"+goodContact+"\n<!-- security-contact:published:end -->\n", 1)
	cases := []struct {
		name, doc string
		wantErr   bool
	}{
		{"pending clean", pendingDoc, false},
		{"pending mailto", pendingDoc + "mailto:team@example.invalid\n", true},
		{"pending bare address", pendingDoc + "team@example.invalid\n", true},
		{"pending labeled URL", pendingDoc + goodContact + "\n", true},
		{"pending placeholder", pendingDoc + "Private security contact: <value>\n", true},
		{"pending list placeholder", pendingDoc + "- Private security contact: <tbd>\n", true},
		{"pending PGP", pendingDoc + "-----BEGIN PGP PUBLIC KEY BLOCK-----\nNOT-A-REAL-KEY\n-----END PGP PUBLIC KEY BLOCK-----\n", true},
		{"pending fingerprint", pendingDoc + fingerprint + "\n", true},
		{"pending long id", pendingDoc + longID + "\n", true},
		{"pending orphan region", pendingDoc + regionStart + "\n" + regionEnd + "\n", true},
		{"pending missing runbook", strings.ReplaceAll(pendingDoc, runbookLink, "other.md"), true},
		{"pending missing notice", strings.ReplaceAll(pendingDoc, maintainerText, "action remains"), true},
		{"pvr operative", pvrDoc, false},
		{"fallback operative", fallbackDoc(goodContact, ""), false},
		{"dual operative", both, false},
		{"duplicate pending", pendingDoc + pendingMarker + "\n", true},
		{"duplicate pvr", pvrDoc + pvrMarker + "\n", true},
		{"duplicate fallback", fallbackDoc(goodContact, "") + "\n<!-- security-channel: fallback-contact decision=" + fallbackRef + " -->\n", true},
		{"fallback no affordance", fallbackDoc("report privately", ""), true},
		{"fallback outside", strings.ReplaceAll(fallbackDoc(goodContact, ""), goodContact+"\n"+regionEnd, regionEnd+"\n"+goodContact), true},
		{"fallback fingerprint outside", fallbackDoc(goodContact, fingerprint), true},
		{"fallback placeholder outside", fallbackDoc(goodContact, "Private security contact: <value>"), true},
		{"fallback no region", strings.ReplaceAll(strings.ReplaceAll(fallbackDoc(goodContact, ""), regionStart+"\n", ""), "\n"+regionEnd, ""), true},
		{"fallback unbalanced", strings.ReplaceAll(fallbackDoc(goodContact, ""), regionEnd, ""), true},
		{"fallback empty", fallbackDoc("", ""), true},
		{"fallback four lines", fallbackDoc(strings.Repeat(goodContact+"\n", 3)+goodContact, ""), true},
		{"fallback prose line", fallbackDoc(goodContact+"\nnot an affordance", ""), true},
		{"notice removed no marker", "No notice and no marker.\n", true},
		{"pending mixed pvr", pendingDoc + pvrMarker + "\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDisclosureChannelConsistency([]byte(tc.doc))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestPendingPlaceholderHitsBothDetectors(t *testing.T) {
	for _, value := range []string{"Private security contact: <value>", "- Private security contact: <tbd>"} {
		doc := []byte(pendingDoc + value + "\n")
		if !ContactAffordancePresent(doc) || !ContactShapedValuePresent(doc) {
			t.Fatal("placeholder carve-out leaked into detector")
		}
		if CheckDisclosureChannelConsistency(doc) == nil {
			t.Fatal("pending placeholder unexpectedly passed")
		}
	}
}

func TestValidateDecisionReference(t *testing.T) {
	valid := []string{fallbackRef, "Tchori-Labs/main:decisions/sub-dir/2026_contact.md"}
	for _, ref := range valid {
		if err := ValidateDecisionReference(ref); err != nil {
			t.Errorf("valid %q: %v", ref, err)
		}
	}
	invalid := []string{
		"Tchori-Labs/main:decisions/../README.md", "Tchori-Labs/main:decisions/./x.md",
		"Tchori-Labs/main:decisions//x.md", "Tchori-Labs/main:decisions/sub/../../secret.md",
		"Tchori-Labs/main:decisions/.md", "Tchori-Labs/main:decisions/x.md/",
		"Tchori-Labs/main:decisions/%2e%2e/x.md", "Tchori-Labs/main:/decisions/x.md",
		"Tchori-Labs/main:decisions/x y.md", `Tchori-Labs/main:decisions\x.md`,
		"Tchori-Labs/main:decisions/EXAMPLE-security-contact.md",
		"Tchori-Labs/main:decisions/team@example.invalid.md",
		"Tchori-Labs/main:decisions/mailto:team.md",
	}
	for _, ref := range invalid {
		if err := ValidateDecisionReference(ref); err == nil {
			t.Errorf("invalid reference passed: %q", ref)
		}
	}
}

func TestNoSyntheticContact(t *testing.T) {
	if CheckNoSyntheticContact([]byte("team@example.invalid")) == nil {
		t.Error("placeholder domain passed")
	}
	plausible := "team" + "@" + "security.tchori.dev"
	if err := CheckNoSyntheticContact([]byte(plausible)); err != nil {
		t.Errorf("plausible domain failed: %v", err)
	}
}

func TestCheckPolicyLinked(t *testing.T) {
	if err := CheckPolicyLinked([]byte("[policy](SECURITY.md) [runbook](docs/security-disclosure-channel.md)")); err != nil {
		t.Fatal(err)
	}
	if CheckPolicyLinked([]byte("[policy](SECURITY.md)")) == nil {
		t.Error("missing runbook passed")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}
