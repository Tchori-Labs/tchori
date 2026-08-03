package sensitive

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRedactJSON(t *testing.T) {
	in := json.RawMessage(`{"client_secret":"secret","big":123456789012345678901234567890,"rules":[{"token":"literal"},{"token":"secret"}]}`)
	out, changed, err := RedactJSON(in, []string{"client_secret", "rules.token"}, []string{"rules[0].token"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`"secret"`)) {
		t.Fatalf("secret remains: %s", out)
	}
	if !bytes.Contains(out, []byte(`"token":"literal"`)) {
		t.Fatalf("literal removed: %s", out)
	}
	if !bytes.Contains(out, []byte(`123456789012345678901234567890`)) {
		t.Fatalf("number changed: %s", out)
	}
	if len(changed) != 2 {
		t.Fatalf("changed = %v", changed)
	}
}

func TestRedactJSONAlreadyNullUnchanged(t *testing.T) {
	_, changed, err := RedactJSON(json.RawMessage(`{"token":null}`), []string{"token"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v", changed)
	}
}

func TestRedactJSONExplicitNilExemptions(t *testing.T) {
	in := json.RawMessage(`{"rules":[{"token":"literal"}]}`)
	out, _, _ := RedactJSON(in, []string{"rules.token"}, nil)
	if bytes.Contains(out, []byte("literal")) {
		t.Fatal("nil exemptions leaked literal")
	}
	out, _, _ = RedactJSON(in, []string{"rules.token"}, []string{"rules[0].token"})
	if !bytes.Contains(out, []byte("literal")) {
		t.Fatal("instance exemption not honored")
	}
}
