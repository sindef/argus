package sealed

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/scrypt"
)

var magic = []byte("ARGUSBK1")

func key(passphrase string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32)
}

func Seal(passphrase string, plain []byte) ([]byte, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	k, err := key(passphrase, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	out := append(append(append([]byte{}, magic...), salt...), nonce...)
	return gcm.Seal(out, nonce, plain, magic), nil
}

func Open(passphrase string, data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, magic) || len(data) < len(magic)+16+12 {
		return nil, errors.New("not an encrypted Argus backup")
	}
	salt := data[len(magic) : len(magic)+16]
	k, err := key(passphrase, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	rest := data[len(magic)+16:]
	plain, err := gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], magic)
	if err != nil {
		return nil, errors.New("wrong passphrase or damaged backup")
	}
	return plain, nil
}
