package buildinfo

import "testing"

func TestCurrentAlwaysHasSafeFallbacks(t *testing.T) {
	info := Current()
	if info.Version == "" || info.Revision == "" {
		t.Fatalf("build info = %#v", info)
	}
}
