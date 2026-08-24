package crypto

import (
	"strings"
	"testing"
)

func TestPasswordHasherUsesArgon2idAndVerifies(t *testing.T) {
	hasher := PasswordHasher{}
	encoded, err := hasher.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") || strings.Contains(encoded, "correct horse") {
		t.Fatalf("unsafe encoded password = %q", encoded)
	}
	ok, err := hasher.Verify("correct horse battery staple", encoded)
	if err != nil || !ok {
		t.Fatalf("Verify(correct) = %v, %v", ok, err)
	}
	ok, err = hasher.Verify("wrong password", encoded)
	if err != nil || ok {
		t.Fatalf("Verify(wrong) = %v, %v", ok, err)
	}
}

func TestPasswordHasherRejectsMalformedParameters(t *testing.T) {
	if _, err := (PasswordHasher{}).Verify("password", "$argon2id$v=19$m=99999999,t=3,p=2$c2FsdA$a2V5"); err == nil {
		t.Fatal("Verify() accepted unsafe parameters")
	}
}
