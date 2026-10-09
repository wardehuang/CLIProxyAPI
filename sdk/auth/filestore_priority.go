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

// BEGIN xAI Guardian core extension: priority-only auth metadata update.
// UpdatePriorityOnly atomically changes only the priority JSON field and verifies it from disk.
func (s *FileTokenStore) UpdatePriorityOnly(ctx context.Context, credential *cliproxyAuth.Auth, priority int) error {
	if credential == nil {
		return fmt.Errorf("auth filestore: auth is nil")
	}
	if cliproxyAuth.IsPluginVirtualAuth(credential) || cliproxyAuth.IsConfigAPIKeyAuth(credential) || credential.Attributes["runtime_only"] == "true" {
		return fmt.Errorf("auth filestore: credential is not a physical file auth")
	}
	if credential.Attributes[cliproxyAuth.AttributeSourceBackend] != cliproxyAuth.AuthSourceFile {
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
	updated, err := sjson.SetBytes(data, "priority", priority)
	if err != nil {
		return fmt.Errorf("auth filestore: set priority: %w", err)
	}
	if !json.Valid(updated) {
		return fmt.Errorf("auth filestore: priority update produced invalid JSON")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".priority-*")
	if err != nil {
		return fmt.Errorf("auth filestore: create priority temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if err := tempFile.Chmod(info.Mode().Perm()); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("auth filestore: set priority temp file mode: %w", err)
	}
	if _, err := tempFile.Write(updated); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("auth filestore: write priority temp file: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("auth filestore: sync priority temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("auth filestore: close priority temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("auth filestore: replace auth priority: %w", err)
	}

	persisted, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("auth filestore: read back auth priority: %w", err)
	}
	var persistedMetadata map[string]json.RawMessage
	if err := json.Unmarshal(persisted, &persistedMetadata); err != nil {
		return fmt.Errorf("auth filestore: decode read-back auth file: %w", err)
	}
	var persistedPriority int
	if err := json.Unmarshal(persistedMetadata["priority"], &persistedPriority); err != nil {
		return fmt.Errorf("auth filestore: decode read-back priority: %w", err)
	}
	if persistedPriority != priority {
		return fmt.Errorf("auth filestore: read-back priority mismatch")
	}
	return nil
}
// END xAI Guardian core extension.
