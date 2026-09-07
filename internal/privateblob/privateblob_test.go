package privateblob

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testKey(seed byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func TestValidateKey(t *testing.T) {
	for name, value := range map[string]string{
		"missing":      "",
		"not base64":   "not-base64",
		"wrong length": base64.StdEncoding.EncodeToString(make([]byte, 31)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(KeyEnv, value)
			if err := ValidateKey(); err == nil {
				t.Fatal("ValidateKey accepted an unusable artifact key")
			}
		})
	}

	t.Setenv(KeyEnv, testKey(1))
	if err := ValidateKey(); err != nil {
		t.Fatalf("ValidateKey = %v", err)
	}
}

func TestSealOpenAuthenticatedRoundTrip(t *testing.T) {
	const secret = "opaque-provider-private-sentinel" //nolint:gosec // synthetic test payload, not a credential
	const context = "state\x00resource.example\x00provider\x00resource"
	t.Setenv(KeyEnv, testKey(2))

	first, err := Seal([]byte(secret), context)
	if err != nil {
		t.Fatalf("Seal = %v", err)
	}
	if bytes.Contains(first, []byte(secret)) || bytes.Contains(first, []byte(base64.StdEncoding.EncodeToString([]byte(secret)))) {
		t.Fatal("sealed envelope exposed private bytes")
	}
	got, err := Open(first, context)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if !bytes.Equal(got, []byte(secret)) {
		t.Fatalf("Open returned %q, want exact opaque bytes", got)
	}

	if _, err := Open(first, context+"-other"); err == nil {
		t.Fatal("Open accepted an envelope in a different authenticated context")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatal("authentication error exposed private content")
	}

	t.Setenv(KeyEnv, testKey(3))
	if _, err := Open(first, context); err == nil {
		t.Fatal("Open accepted the wrong key")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatal("wrong-key error exposed private content")
	}
}

func TestOpenRejectsMalformedAndTamperedEnvelope(t *testing.T) {
	t.Setenv(KeyEnv, testKey(4))
	sealed, err := Seal([]byte("do-not-disclose"), "plan\x00thing.one\x00test\x00thing")
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(sealed, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["ciphertext"] = base64.StdEncoding.EncodeToString([]byte("tampered"))
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(tampered, "plan\x00thing.one\x00test\x00thing"); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}

	envelope["nonce"] = base64.StdEncoding.EncodeToString([]byte("short"))
	malformed, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(malformed, "plan\x00thing.one\x00test\x00thing"); err == nil {
		t.Fatal("Open accepted a malformed nonce")
	}
}
