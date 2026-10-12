package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func key(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestVault_RoundTrip(t *testing.T) {
	v, err := New(key(t, 32))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plain := []byte("1//0g-refresh-token-of-a-user")
	ct, err := v.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("the ciphertext contains the plaintext")
	}
	got, err := v.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Decrypt = %q, want %q", got, plain)
	}

	empty, err := v.Encrypt(nil)
	if err != nil {
		t.Fatalf("Encrypt(nil): %v", err)
	}
	if got, err := v.Decrypt(empty); err != nil || len(got) != 0 {
		t.Fatalf("Decrypt(Encrypt(nil)) = %q, %v; want empty", got, err)
	}
}

func TestVault_TamperedCiphertextFails(t *testing.T) {
	v, err := New(key(t, 32))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := v.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	flip := func(i int) []byte {
		c := bytes.Clone(ct)
		c[i] ^= 0x01
		return c
	}
	for name, bad := range map[string][]byte{
		"nonce bit flipped":      flip(0),
		"ciphertext bit flipped": flip(len(ct) - 1),
		"truncated":              ct[:len(ct)-1],
		"shorter than the nonce": ct[:3],
		"empty":                  nil,
	} {
		if got, err := v.Decrypt(bad); err == nil {
			t.Errorf("%s: Decrypt succeeded with %q", name, got)
		}
	}

	other, err := New(key(t, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.Decrypt(ct); err == nil {
		t.Errorf("a different key decrypted the ciphertext to %q", got)
	}
}

func TestVault_RejectsWrongKeyLength(t *testing.T) {
	for name, k := range map[string]string{
		"empty":         "",
		"16 bytes":      key(t, 16),
		"31 bytes":      key(t, 31),
		"33 bytes":      key(t, 33),
		"not base64":    "this is not base64 !!!",
		"url alphabet":  strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 16))),
		"raw 32 chars":  strings.Repeat("k", 32),
		"unpadded b64u": base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
	} {
		if _, err := New(k); err == nil {
			t.Errorf("%s: New accepted the key", name)
		}
	}
}

func TestVault_ErrorsNeverEchoTheKey(t *testing.T) {
	const k = "c2VjcmV0LXNob3J0LWtleQ==" // base64 of a 16-byte secret
	_, err := New(k)
	if err == nil {
		t.Fatal("New accepted a 16-byte key")
	}
	if strings.Contains(err.Error(), k) {
		t.Fatalf("error echoes the key: %v", err)
	}
}
