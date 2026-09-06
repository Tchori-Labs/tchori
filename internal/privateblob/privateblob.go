// Package privateblob protects engine-owned opaque payloads in persisted artifacts.
package privateblob

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// KeyEnv names the environment-only AES-256 key used for artifact encryption.
const KeyEnv = "TCHORI_ARTIFACT_KEY"

const envelopeVersion = 1

var errAuthentication = errors.New("private artifact authentication failed")

type envelope struct {
	Version    int    `json:"version"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// ValidateKey verifies that TCHORI_ARTIFACT_KEY is standard-base64 encoded and
// decodes to exactly 32 bytes. The key is never persisted or synthesized.
func ValidateKey() error {
	_, err := key()
	return err
}

// Seal encrypts plaintext with AES-256-GCM and authenticates context as
// additional data. Each envelope uses a fresh random nonce.
func Seal(plaintext []byte, context string) ([]byte, error) {
	if context == "" {
		return nil, errors.New("private artifact context is required")
	}
	keyBytes, err := key()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(keyBytes)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("generate private artifact nonce")
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(context))
	encoded, err := json.Marshal(envelope{Version: envelopeVersion, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return nil, errors.New("encode private artifact envelope")
	}
	return encoded, nil
}

// Open authenticates and decrypts an envelope in the exact context supplied to
// Seal. Authentication errors intentionally disclose neither plaintext nor
// cryptographic details.
func Open(encoded []byte, context string) ([]byte, error) {
	if context == "" {
		return nil, errors.New("private artifact context is required")
	}
	keyBytes, err := key()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(keyBytes)
	if err != nil {
		return nil, err
	}
	var env envelope
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil {
		return nil, errors.New("invalid private artifact envelope")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("invalid private artifact envelope")
	}
	if env.Version != envelopeVersion || len(env.Nonce) != gcm.NonceSize() || len(env.Ciphertext) < gcm.Overhead() {
		return nil, errors.New("invalid private artifact envelope")
	}
	plaintext, err := gcm.Open(nil, env.Nonce, env.Ciphertext, []byte(context))
	if err != nil {
		return nil, errAuthentication
	}
	return plaintext, nil
}

func key() ([]byte, error) {
	encoded := os.Getenv(KeyEnv)
	if encoded == "" {
		return nil, fmt.Errorf("%s is required for private artifact encryption", KeyEnv)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s must be standard base64 encoding of 32 bytes", KeyEnv)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("%s must decode to exactly 32 bytes", KeyEnv)
	}
	return decoded, nil
}

func newGCM(keyBytes []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, errors.New("initialize private artifact cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize private artifact authentication")
	}
	return gcm, nil
}
