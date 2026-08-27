package infrastructure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"lidradar/backend/platform/ids"
)

type NodeCredentials struct {
	NodeID     string `json:"nodeId"`
	NodeSecret string `json:"nodeSecret"`
}

// LoadNodeCredentials reads the only long-lived secret file of the disposable
// node. Group/world permissions are rejected before contents are decoded.
func LoadNodeCredentials(path string) (NodeCredentials, error) {
	if path == "" {
		return NodeCredentials{}, errors.New("AI node credentials file is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return NodeCredentials{}, fmt.Errorf("read AI node credentials metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return NodeCredentials{}, errors.New("AI node credentials file must be regular and readable only by its owner")
	}
	file, err := os.Open(path)
	if err != nil {
		return NodeCredentials{}, fmt.Errorf("open AI node credentials: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(contents) > 4096 {
		return NodeCredentials{}, errors.New("AI node credentials file is invalid")
	}
	var credentials NodeCredentials
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!ids.Valid(credentials.NodeID) || len(credentials.NodeSecret) < 32 || len(credentials.NodeSecret) > 200 {
		return NodeCredentials{}, errors.New("AI node credentials file is invalid")
	}
	return credentials, nil
}

// WriteNodeCredentials creates a new owner-only file and never overwrites an
// existing secret. The caller is responsible for moving it to the AI node over
// a trusted channel.
func WriteNodeCredentials(path string, credentials NodeCredentials) error {
	if path == "" || !ids.Valid(credentials.NodeID) || len(credentials.NodeSecret) < 32 || len(credentials.NodeSecret) > 200 {
		return errors.New("AI node credentials are invalid")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create AI credentials directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create AI node credentials file: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(credentials); err != nil {
		return errors.New("write AI node credentials file")
	}
	if err := file.Sync(); err != nil {
		return errors.New("synchronize AI node credentials file")
	}
	if err := file.Close(); err != nil {
		return errors.New("close AI node credentials file")
	}
	remove = false
	return nil
}
