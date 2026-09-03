package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "password.txt")
	first, err := readOrCreatePassword(path)
	if err != nil || len(first) != 32 {
		t.Fatal("password creation failed")
	}
	second, err := readOrCreatePassword(path)
	if err != nil || second != first {
		t.Fatal("existing password overwritten")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("incorrect password permissions")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readOrCreatePassword(path); err == nil {
		t.Fatal("public password file accepted")
	}
	link := filepath.Join(t.TempDir(), "password-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readOrCreatePassword(link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestCLIRejectsUnsafeRequestsBeforeConnecting(t *testing.T) {
	t.Setenv("LIDRADAR_ENV", "production")
	t.Setenv("LIDRADAR_DATABASE_URL", "postgres://unused/lidradar_frontend")
	for _, args := range [][]string{nil, {"unknown"}, {"up"}, {"status"}, {"down"}, {"down", "-confirm", "wrong"}} {
		if err := run(args); err == nil {
			t.Fatalf("unsafe command accepted: %v", args)
		}
	}
}
