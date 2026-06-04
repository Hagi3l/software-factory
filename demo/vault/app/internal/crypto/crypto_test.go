package crypto

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := DeriveKey("correct horse battery staple", mustSalt(t))
	msg := []byte("s3cr3t-value")

	blob, err := Seal(key, msg)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(blob, msg) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := Open(key, blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("roundtrip mismatch: got %q", got)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	salt := mustSalt(t)
	blob, err := Seal(DeriveKey("right", salt), []byte("data"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := Open(DeriveKey("wrong", salt), blob); err != ErrDecrypt {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestOpenTamperedFails(t *testing.T) {
	key := DeriveKey("pw", mustSalt(t))
	blob, err := Seal(key, []byte("data"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	blob[len(blob)-1] ^= 0xff // flip a ciphertext bit
	if _, err := Open(key, blob); err != ErrDecrypt {
		t.Fatalf("want ErrDecrypt on tamper, got %v", err)
	}
}

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("hunter2hunter2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !VerifyPassword("hunter2hunter2", h) {
		t.Fatal("correct password did not verify")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("wrong password verified")
	}
	if VerifyPassword("hunter2hunter2", "garbage") {
		t.Fatal("malformed verifier accepted")
	}
}

func TestKeyLength(t *testing.T) {
	if got := len(DeriveKey("x", mustSalt(t))); got != KeyLen {
		t.Fatalf("derived key length = %d, want %d", got, KeyLen)
	}
}

func mustSalt(t *testing.T) []byte {
	t.Helper()
	salt, err := NewSalt()
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	return salt
}
