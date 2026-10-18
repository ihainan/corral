package vault

import (
	"bytes"
	"testing"
)

func TestSealOpenAndBinding(t *testing.T) {
	v, _ := New(bytes.Repeat([]byte{1}, 32))
	s, err := v.Seal([]byte("secret"), []byte("row:1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.Open(s, []byte("row:1")); err != nil || string(got) != "secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	// A ciphertext copied onto another row must not decrypt.
	if _, err := v.Open(s, []byte("row:2")); err == nil {
		t.Fatal("ciphertext opened under a different row binding")
	}
	// A different master key cannot open it.
	other, _ := New(bytes.Repeat([]byte{2}, 32))
	if _, err := other.Open(s, []byte("row:1")); err == nil {
		t.Fatal("opened with the wrong master key")
	}
	// Rotation: a vault that lists the old key as previous still opens old secrets.
	rotated, _ := New(bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{1}, 32))
	if got, err := rotated.Open(s, []byte("row:1")); err != nil || string(got) != "secret" {
		t.Fatalf("rotated open: %q %v", got, err)
	}
}
