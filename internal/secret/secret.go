// Package secret encrypts account credentials at rest with AES-256-GCM.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
)

type Box struct{ aead cipher.AEAD }

// New derives a 256-bit key from any passphrase (sha256).
func New(passphrase string) (*Box, error) {
	if passphrase == "" {
		return nil, errors.New("RELAY_ENCRYPTION_KEY is empty")
	}
	key := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, nil), nil
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

func (b *Box) SealJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b.Seal(raw)
}

func (b *Box) OpenJSON(sealed []byte, v any) error {
	raw, err := b.Open(sealed)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}
