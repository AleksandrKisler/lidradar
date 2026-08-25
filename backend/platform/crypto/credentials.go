package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

var ErrCredentials = errors.New("не удалось обработать зашифрованные реквизиты")

const credentialCipherVersion byte = 1

// CredentialCipher шифрует реквизиты AES-256-GCM и связывает шифротекст с
// организацией, поставщиком и подключением через дополнительные данные.
type CredentialCipher struct {
	aead   cipher.AEAD
	random io.Reader
}

// NewCredentialCipher создаёт шифратор из ровно 32 случайных байтов ключа.
func NewCredentialCipher(key []byte) (CredentialCipher, error) {
	if len(key) != 32 {
		return CredentialCipher{}, ErrCredentials
	}
	block, err := aes.NewCipher(append([]byte(nil), key...))
	if err != nil {
		return CredentialCipher{}, ErrCredentials
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return CredentialCipher{}, ErrCredentials
	}
	return CredentialCipher{aead: aead, random: rand.Reader}, nil
}

// Encrypt создаёт версионированный шифротекст со случайным одноразовым числом.
func (ciphertext CredentialCipher) Encrypt(plaintext, additionalData []byte) ([]byte, error) {
	if ciphertext.aead == nil || ciphertext.random == nil || len(plaintext) == 0 || len(additionalData) == 0 {
		return nil, ErrCredentials
	}
	nonce := make([]byte, ciphertext.aead.NonceSize())
	if _, err := io.ReadFull(ciphertext.random, nonce); err != nil {
		return nil, ErrCredentials
	}
	result := make([]byte, 1, 1+len(nonce)+len(plaintext)+ciphertext.aead.Overhead())
	result[0] = credentialCipherVersion
	result = append(result, nonce...)
	result = ciphertext.aead.Seal(result, nonce, plaintext, additionalData)
	return result, nil
}

// Decrypt проверяет версию, целостность и дополнительные данные шифротекста.
func (ciphertext CredentialCipher) Decrypt(encrypted, additionalData []byte) ([]byte, error) {
	if ciphertext.aead == nil || len(additionalData) == 0 || len(encrypted) < 1+ciphertext.aead.NonceSize()+ciphertext.aead.Overhead() ||
		encrypted[0] != credentialCipherVersion {
		return nil, ErrCredentials
	}
	nonceEnd := 1 + ciphertext.aead.NonceSize()
	plaintext, err := ciphertext.aead.Open(nil, encrypted[1:nonceEnd], encrypted[nonceEnd:], additionalData)
	if err != nil {
		return nil, ErrCredentials
	}
	return plaintext, nil
}
