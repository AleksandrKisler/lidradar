package infrastructure_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lidradar/backend/internal/ai/infrastructure"
	"lidradar/backend/platform/ids"
)

func TestNodeCredentialsRequireOwnerOnlyFileAndNoOverwrite(t *testing.T) {
	nodeID, err := (ids.Generator{}).NewID()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private", "node.json")
	credentials := infrastructure.NodeCredentials{
		NodeID: nodeID, NodeSecret: "secret-with-at-least-32-characters",
	}
	if err := infrastructure.WriteNodeCredentials(path, credentials); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	loaded, err := infrastructure.LoadNodeCredentials(path)
	if err != nil || loaded != credentials {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	if err := infrastructure.WriteNodeCredentials(path, infrastructure.NodeCredentials{
		NodeID: nodeID, NodeSecret: strings.Repeat("x", 40),
	}); err == nil {
		t.Fatal("existing credentials were overwritten")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := infrastructure.LoadNodeCredentials(path); err == nil {
		t.Fatal("world-readable credentials were accepted")
	}
}
