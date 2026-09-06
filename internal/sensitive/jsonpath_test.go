package sensitive

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRedactJSON(t *testing.T) {
	in := json.RawMessage(`{"client_secret":"secret","big":123456789012345678901234567890,"rules":[{"token":"literal"},{"token":"secret"}]}`)
	out, changed, err := RedactJSON(in, []string{"client_secret", "rules.token"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`"secret"`)) {
		t.Fatalf("secret remains: %s", out)
	}
	if bytes.Contains(out, []byte(`"literal"`)) {
		t.Fatalf("provider-free redaction exempted a persisted value: %s", out)
	}
	if !bytes.Contains(out, []byte(`123456789012345678901234567890`)) {
		t.Fatalf("number changed: %s", out)
	}
	if len(changed) != 2 {
		t.Fatalf("changed = %v", changed)
	}
}

func TestRedactJSONAlreadyNullUnchanged(t *testing.T) {
	_, changed, err := RedactJSON(json.RawMessage(`{"token":null}`), []string{"token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v", changed)
	}
}

func TestRedactJSONNeverExemptsPersistedValues(t *testing.T) {
	in := json.RawMessage(`{"rules":[{"token":"literal"}]}`)
	out, _, _ := RedactJSON(in, []string{"rules.token"})
	if bytes.Contains(out, []byte("literal")) {
		t.Fatal("provider-free redaction leaked a value that merely occupied a formerly exempt path")
	}
}
