package token

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRecoverableCredential(t *testing.T) {
	aead, err := Cipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	id, secret, encrypted := Generate(aead)
	if ID(secret) != id || strings.Contains(encrypted, secret) {
		t.Fatal("invalid identity or plaintext storage")
	}
	for range 2 {
		got, err := Reveal(aead, id, encrypted)
		if err != nil || got != secret {
			t.Fatal("credential must remain recoverable")
		}
	}
	if _, err := Reveal(aead, "another-record", encrypted); err == nil {
		t.Fatal("ciphertext must bind to record")
	}
	other, _ := Cipher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if _, err := Reveal(other, id, encrypted); err == nil {
		t.Fatal("wrong key accepted")
	}
	for _, value := range []string{"", " \t\n", strings.Repeat("名", 101)} {
		if ValidateName(value) == nil {
			t.Fatal("invalid name accepted")
		}
	}
	if ValidateName("算法联调") != nil {
		t.Fatal("valid name rejected")
	}
}
