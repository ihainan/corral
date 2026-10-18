// Package vault encrypts stored SSH secrets with envelope encryption.
//
// Each secret gets its own random data key (AES-256-GCM). The data key is wrapped
// with the deployment's master key, and the key id is stored next to it so the
// master key can be rotated later. The associated data binds a ciphertext to the
// row it belongs to, so copying an encrypted blob onto another user's row fails
// to decrypt instead of silently working.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

type Vault struct {
	keys    map[string][]byte
	current string
}

// Sealed is what gets persisted for one secret.
type Sealed struct {
	Ciphertext []byte
	WrappedKey []byte
	KeyID      string
}

func New(master []byte, older ...[]byte) (*Vault, error) {
	if len(master) != 32 {
		return nil, errors.New("vault: master key must be 32 bytes")
	}
	v := &Vault{keys: map[string][]byte{}}
	for _, k := range append(older, master) {
		if len(k) != 32 {
			return nil, errors.New("vault: master key must be 32 bytes")
		}
		v.keys[keyID(k)] = k
	}
	v.current = keyID(master)
	return v, nil
}

func keyID(k []byte) string {
	sum := sha256.Sum256(append([]byte("corral-master-key-id:"), k...))
	return hex.EncodeToString(sum[:6])
}

func (v *Vault) Seal(plaintext, aad []byte) (Sealed, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, err
	}
	ct, err := gcmSeal(dek, plaintext, aad)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := gcmSeal(v.keys[v.current], dek, []byte("dek:"+v.current))
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{Ciphertext: ct, WrappedKey: wrapped, KeyID: v.current}, nil
}

func (v *Vault) Open(s Sealed, aad []byte) ([]byte, error) {
	mk, ok := v.keys[s.KeyID]
	if !ok {
		return nil, fmt.Errorf("vault: secret was sealed with unknown master key %s", s.KeyID)
	}
	dek, err := gcmOpen(mk, s.WrappedKey, []byte("dek:"+s.KeyID))
	if err != nil {
		return nil, errors.New("vault: cannot unwrap data key")
	}
	pt, err := gcmOpen(dek, s.Ciphertext, aad)
	if err != nil {
		return nil, errors.New("vault: cannot decrypt secret")
	}
	return pt, nil
}

func gcmSeal(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, plaintext, aad), nil
}

func gcmOpen(key, sealed, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < g.NonceSize() {
		return nil, errors.New("short ciphertext")
	}
	return g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], aad)
}
