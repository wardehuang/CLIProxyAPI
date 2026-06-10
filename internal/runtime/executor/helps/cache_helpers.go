package helps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CodexPromptCacheTTL is the sliding retention window for persisted prompt cache IDs.
	CodexPromptCacheTTL                    = 24 * time.Hour
	codexCacheFileName                     = "codex_prompt_cache.json"
	codexClaudeSessionProjectCacheFileName = "codex_claude_session_projects.json"
)

type CodexCache struct {
	ID     string    `json:"id"`
	Expire time.Time `json:"expire"`
}

type CodexClaudeSessionProject struct {
	ProjectID string    `json:"project_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

type codexCacheFile struct {
	Entries map[string]CodexCache `json:"entries"`
}

type codexClaudeSessionProjectCacheFile struct {
	Entries map[string]CodexClaudeSessionProject `json:"entries"`
}

var codexCacheFileMu sync.Mutex

// GetAndRefreshCodexCache retrieves a persisted cache entry and extends its expiry.
func GetAndRefreshCodexCache(baseDir, key string, ttl time.Duration) (CodexCache, bool, error) {
	if ttl <= 0 {
		ttl = CodexPromptCacheTTL
	}
	codexCacheFileMu.Lock()
	defer codexCacheFileMu.Unlock()

	storePath, err := codexCachePath(baseDir)
	if err != nil {
		return CodexCache{}, false, err
	}
	store, err := loadCodexCacheFile(storePath)
	if err != nil {
		return CodexCache{}, false, err
	}

	now := time.Now()
	purged := purgeExpiredCodexCacheEntries(store.Entries, now)
	cache, ok := store.Entries[key]
	if !ok {
		if purged {
			if err = saveCodexCacheFile(storePath, store); err != nil {
				return CodexCache{}, false, err
			}
		}
		return CodexCache{}, false, nil
	}
	if !cache.Expire.After(now) {
		delete(store.Entries, key)
		if err = saveCodexCacheFile(storePath, store); err != nil {
			return CodexCache{}, false, err
		}
		return CodexCache{}, false, nil
	}
	cache.Expire = now.Add(ttl)
	store.Entries[key] = cache
	if err = saveCodexCacheFile(storePath, store); err != nil {
		return CodexCache{}, false, err
	}
	return cache, true, nil
}

// SetCodexCache stores a cache entry on disk.
func SetCodexCache(baseDir, key string, cache CodexCache) error {
	codexCacheFileMu.Lock()
	defer codexCacheFileMu.Unlock()

	storePath, err := codexCachePath(baseDir)
	if err != nil {
		return err
	}
	store, err := loadCodexCacheFile(storePath)
	if err != nil {
		return err
	}
	purgeExpiredCodexCacheEntries(store.Entries, time.Now())
	store.Entries[key] = cache
	return saveCodexCacheFile(storePath, store)
}

// GetCodexClaudeSessionProject retrieves a persisted Claude Code session to project binding.
func GetCodexClaudeSessionProject(baseDir, sessionID string) (string, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", false, nil
	}
	codexCacheFileMu.Lock()
	defer codexCacheFileMu.Unlock()

	storePath, err := codexClaudeSessionProjectCachePath(baseDir)
	if err != nil {
		return "", false, err
	}
	store, err := loadCodexClaudeSessionProjectCacheFile(storePath)
	if err != nil {
		return "", false, err
	}
	entry, ok := store.Entries[sessionID]
	if !ok || strings.TrimSpace(entry.ProjectID) == "" {
		return "", false, nil
	}
	return strings.TrimSpace(entry.ProjectID), true, nil
}

// SetCodexClaudeSessionProject stores a Claude Code session to project binding on disk.
func SetCodexClaudeSessionProject(baseDir, sessionID, projectID string) error {
	sessionID = strings.TrimSpace(sessionID)
	projectID = strings.TrimSpace(projectID)
	if sessionID == "" || projectID == "" {
		return nil
	}
	codexCacheFileMu.Lock()
	defer codexCacheFileMu.Unlock()

	storePath, err := codexClaudeSessionProjectCachePath(baseDir)
	if err != nil {
		return err
	}
	store, err := loadCodexClaudeSessionProjectCacheFile(storePath)
	if err != nil {
		return err
	}
	store.Entries[sessionID] = CodexClaudeSessionProject{ProjectID: projectID, UpdatedAt: time.Now()}
	return saveCodexClaudeSessionProjectCacheFile(storePath, store)
}

func codexCachePath(baseDir string) (string, error) {
	if baseDir == "" {
		return "", fmt.Errorf("codex prompt cache: base directory is empty")
	}
	return filepath.Join(baseDir, "cache", codexCacheFileName), nil
}

func codexClaudeSessionProjectCachePath(baseDir string) (string, error) {
	if baseDir == "" {
		return "", fmt.Errorf("codex Claude session project cache: base directory is empty")
	}
	return filepath.Join(baseDir, "cache", codexClaudeSessionProjectCacheFileName), nil
}

func loadCodexCacheFile(path string) (codexCacheFile, error) {
	store := codexCacheFile{Entries: make(map[string]CodexCache)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return store, fmt.Errorf("read codex prompt cache: %w", err)
	}
	if len(data) == 0 {
		return store, nil
	}
	if err = json.Unmarshal(data, &store); err != nil {
		return codexCacheFile{Entries: make(map[string]CodexCache)}, fmt.Errorf("parse codex prompt cache: %w", err)
	}
	if store.Entries == nil {
		store.Entries = make(map[string]CodexCache)
	}
	return store, nil
}

func saveCodexCacheFile(path string, store codexCacheFile) error {
	if store.Entries == nil {
		store.Entries = make(map[string]CodexCache)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create codex prompt cache dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex prompt cache: %w", err)
	}
	tmpPath := path + ".tmp"
	if err = os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write codex prompt cache: %w", err)
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("remove old codex prompt cache: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace codex prompt cache: %w", err)
	}
	return nil
}

func loadCodexClaudeSessionProjectCacheFile(path string) (codexClaudeSessionProjectCacheFile, error) {
	store := codexClaudeSessionProjectCacheFile{Entries: make(map[string]CodexClaudeSessionProject)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return store, fmt.Errorf("read codex Claude session project cache: %w", err)
	}
	if len(data) == 0 {
		return store, nil
	}
	if err = json.Unmarshal(data, &store); err != nil {
		return codexClaudeSessionProjectCacheFile{Entries: make(map[string]CodexClaudeSessionProject)}, fmt.Errorf("parse codex Claude session project cache: %w", err)
	}
	if store.Entries == nil {
		store.Entries = make(map[string]CodexClaudeSessionProject)
	}
	return store, nil
}

func saveCodexClaudeSessionProjectCacheFile(path string, store codexClaudeSessionProjectCacheFile) error {
	if store.Entries == nil {
		store.Entries = make(map[string]CodexClaudeSessionProject)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create codex Claude session project cache dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex Claude session project cache: %w", err)
	}
	tmpPath := path + ".tmp"
	if err = os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write codex Claude session project cache: %w", err)
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("remove old codex Claude session project cache: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace codex Claude session project cache: %w", err)
	}
	return nil
}

func purgeExpiredCodexCacheEntries(entries map[string]CodexCache, now time.Time) bool {
	purged := false
	for key, cache := range entries {
		if !cache.Expire.After(now) {
			delete(entries, key)
			purged = true
		}
	}
	return purged
}
