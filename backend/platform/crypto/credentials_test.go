package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestCredentialCipherRoundTripAndBinding(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	ciphertext, err := NewCredentialCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"botToken":"123456:secret-token-value"}`)
	additionalData := []byte("tenant:telegram:connection")
	first, err := ciphertext.Encrypt(plaintext, additionalData)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ciphertext.Encrypt(plaintext, additionalData)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, plaintext) || bytes.Equal(first, second) {
		t.Fatal("шифрование не скрывает текст либо повторно использует одноразовое число")
	}
	decoded, err := ciphertext.Decrypt(first, additionalData)
	if err != nil || !bytes.Equal(decoded, plaintext) {
		t.Fatalf("Decrypt() = %q, %v", decoded, err)
	}
	if _, err := ciphertext.Decrypt(first, []byte("другое подключение")); !errors.Is(err, ErrCredentials) {
		t.Fatalf("Decrypt() с чужой привязкой = %v", err)
	}
	first[len(first)-1] ^= 0xff
	if _, err := ciphertext.Decrypt(first, additionalData); !errors.Is(err, ErrCredentials) {
		t.Fatalf("Decrypt() изменённого шифротекста = %v", err)
	}
}

func TestCredentialCipherRejectsWrongKeyLength(t *testing.T) {
	if _, err := NewCredentialCipher([]byte("короткий ключ")); !errors.Is(err, ErrCredentials) {
		t.Fatalf("NewCredentialCipher() = %v", err)
	}
}
