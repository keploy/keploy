package replay

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
)

// helpers

func makeKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}

func encryptPlaintext(plain string, key []byte) string {
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encryptedPrefix + "test-key-id:" + base64.StdEncoding.EncodeToString(ct)
}

// parseKeyID tests

func TestParseKeyID_Valid(t *testing.T) {
	enc := "ENC:my-key-123:somebase64data"
	got := parseKeyID(enc)
	if got != "my-key-123" {
		t.Fatalf("expected my-key-123, got %q", got)
	}
}

func TestParseKeyID_NotEncrypted(t *testing.T) {
	if got := parseKeyID("plaintext"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestParseKeyID_MissingColon(t *testing.T) {
	if got := parseKeyID("ENC:nokeyidseparator"); got != "" {
		t.Fatalf("expected empty for malformed, got %q", got)
	}
}

// decryptValue tests

func TestDecryptValue_Roundtrip(t *testing.T) {
	key := makeKey()
	enc := encryptPlaintext("secret-value", key)
	got, err := decryptValue(enc, key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "secret-value" {
		t.Fatalf("expected secret-value, got %q", got)
	}
}

func TestDecryptValue_NotEncrypted(t *testing.T) {
	got, err := decryptValue("plaintext", makeKey())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "plaintext" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestDecryptValue_WrongKey(t *testing.T) {
	enc := encryptPlaintext("secret", makeKey())
	_, err := decryptValue(enc, makeKey()) // different key
	if err == nil {
		t.Fatal("expected GCM auth error with wrong key")
	}
}

func TestDecryptValue_WrongKeyLength(t *testing.T) {
	_, err := decryptValue("ENC:kid:"+base64.StdEncoding.EncodeToString([]byte("tooshort")), []byte("tooshort"))
	if err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}

func TestDecryptValue_MalformedBase64(t *testing.T) {
	_, err := decryptValue("ENC:kid:!!!notbase64!!!", makeKey())
	if err == nil {
		t.Fatal("expected base64 decode error")
	}
}

func TestDecryptValue_CiphertextTooShort(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("tiny"))
	_, err := decryptValue("ENC:kid:"+short, makeKey())
	if err == nil {
		t.Fatal("expected ciphertext-too-short error")
	}
}

// kmsKeyCache.decryptField tests

func TestDecryptField_NotEncrypted(t *testing.T) {
	c := newKMSKeyCache("http://localhost", "")
	got, err := c.decryptField(context.Background(), "plain")
	if err != nil || got != "plain" {
		t.Fatalf("expected passthrough, got %q %v", got, err)
	}
}

func TestDecryptField_MalformedENC(t *testing.T) {
	c := newKMSKeyCache("http://localhost", "")
	_, err := c.decryptField(context.Background(), "ENC:nokeyid")
	if err == nil {
		t.Fatal("expected error for malformed ENC value")
	}
}

func TestDecryptField_CachedKey(t *testing.T) {
	key := makeKey()
	enc := encryptPlaintext("hello", key)
	keyID := parseKeyID(enc)

	c := newKMSKeyCache("http://localhost", "")
	// pre-seed the cache so no HTTP call is needed
	c.keys[keyID] = key

	got, err := c.decryptField(context.Background(), enc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Fatalf("expected hello, got %q", got)
	}
}

func TestDecryptField_GCMAuthFailure(t *testing.T) {
	enc := encryptPlaintext("secret", makeKey())
	keyID := parseKeyID(enc)

	c := newKMSKeyCache("http://localhost", "")
	c.keys[keyID] = makeKey() // wrong key in cache

	_, err := c.decryptField(context.Background(), enc)
	if err == nil {
		t.Fatal("expected GCM auth tag failure")
	}
}

// isEncrypted tests

func TestIsEncrypted(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ENC:kid:data", true},
		{"plaintext", false},
		{"", false},
		{fmt.Sprintf("%s", encryptedPrefix), true},
	}
	for _, tc := range cases {
		if got := isEncrypted(tc.in); got != tc.want {
			t.Errorf("isEncrypted(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
