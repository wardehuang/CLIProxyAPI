package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	cliproxyAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/sjson"
)

// BEGIN xAI Guardian core extension: narrow auth metadata persistence.
// UpdatePriorityOnly atomically changes only the priority JSON field and verifies it from disk.
func (s *FileTokenStore) UpdatePriorityOnly(ctx context.Context, credential *cliproxyAuth.Auth, priority int) error {
	return s.updateMetadataFieldOnly(ctx, credential, "priority", priority)
}

// UpdateMetadataStringOnly atomically changes one supported string metadata field and verifies it from disk.
func (s *FileTokenStore) UpdateMetadataStringOnly(ctx context.Context, credential *cliproxyAuth.Auth, field, value string) error {
	if field != "proxy_url" && field != "refresh_proxy_url" {
		return fmt.Errorf("auth filestore: unsupported string metadata field %q", field)
	}
	return s.updateMetadataFieldOnly(ctx, credential, field, value)
}

func (s *FileTokenStore) updateMetadataFieldOnly(ctx context.Context, credential *cliproxyAuth.Auth, field string, value any) error {
	if credential == nil {
		return fmt.Errorf("auth filestore: auth is nil")
	}
	if cliproxyAuth.IsPluginVirtualAuth(credential) || cliproxyAuth.IsConfigAPIKeyAuth(credential) || credential.Attributes["runtime_only"] == "true" {
		return fmt.Errorf("auth filestore: credential is not a physical file auth")
	}
	if credential.AuthSourceKind() != cliproxyAuth.AuthSourceFile {
		return fmt.Errorf("auth filestore: credential is not file-backed")
	}
	path, err := s.resolveAuthPath(credential)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("auth filestore: stat auth file: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("auth filestore: read auth file: %w", err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("auth filestore: decode auth file: %w", err)
	}
	if metadata == nil {
		return fmt.Errorf("auth filestore: auth file root must be an object")
	}
	updated, err := sjson.SetBytes(data, field, value)
	if err != nil {
		return fmt.Errorf("auth filestore: set %s: %w", field, err)
	}
	if !json.Valid(updated) {
		return fmt.Errorf("auth filestore: %s update produced invalid JSON", field)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".metadata-*")
	if err != nil {
		return fmt.Errorf("auth filestore: create metadata temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if err := tempFile.Chmod(info.Mode().Perm()); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("auth filestore: set metadata temp file mode: %w", err)
	}
	if _, err := tempFile.Write(updated); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("auth filestore: write metadata temp file: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("auth filestore: sync metadata temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("auth filestore: close metadata temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("auth filestore: replace auth metadata: %w", err)
	}

	persisted, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("auth filestore: read back auth metadata: %w", err)
	}
	var persistedMetadata map[string]json.RawMessage
	if err := json.Unmarshal(persisted, &persistedMetadata); err != nil {
		return fmt.Errorf("auth filestore: decode read-back auth file: %w", err)
	}
	expected, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("auth filestore: encode expected %s: %w", field, err)
	}
	if string(persistedMetadata[field]) != string(expected) {
		return fmt.Errorf("auth filestore: read-back %s mismatch", field)
	}
	return nil
}

// END xAI Guardian core extension.
