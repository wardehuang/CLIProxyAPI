package helps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CodexPromptCacheTTL is the sliding retention window for persisted prompt cache IDs.
	CodexPromptCacheTTL                   = 24 * time.Hour
	codexCacheDirName                     = "cache"
	codexPromptCacheDirName               = "codex"
	codexCacheFileName                    = "codex_prompt_cache.json"
	codexClaudeSessionProjectFileName     = "codex_claude_session_projects.json"
	codexCursorPromptCacheProjectFileName = "codex_cursor_prompt_cache_projects.json"
)

type CodexCache struct {
	ID     string    `json:"id"`
	Expire time.Time `json:"expire"`
}

type CodexClaudeSessionProject struct {
	ProjectID string    `json:"project_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CodexCursorPromptCacheProject struct {
	ProjectID string    `json:"project_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

type codexCacheFile struct {
	Entries map[string]CodexCache `json:"entries"`
}

type codexClaudeSessionProjectCacheFile struct {
	Entries map[string]CodexClaudeSessionProject `json:"entries"`
}

type codexCursorPromptCacheProjectFile struct {
	Entries map[string]CodexCursorPromptCacheProject `json:"entries"`
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

// GetCodexCursorPromptCacheProject retrieves a persisted Cursor prompt cache key to project binding.
func GetCodexCursorPromptCacheProject(baseDir, promptCacheKey string) (string, bool, error) {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if promptCacheKey == "" {
		return "", false, nil
	}
	codexCacheFileMu.Lock()
	defer codexCacheFileMu.Unlock()

	storePath, err := codexCursorPromptCacheProjectPath(baseDir)
	if err != nil {
		return "", false, err
	}
	store, err := loadCodexCursorPromptCacheProjectFile(storePath)
	if err != nil {
		return "", false, err
	}
	entry, ok := store.Entries[promptCacheKey]
	if !ok || strings.TrimSpace(entry.ProjectID) == "" {
		return "", false, nil
	}
	return strings.TrimSpace(entry.ProjectID), true, nil
}

// SetCodexCursorPromptCacheProject stores a Cursor prompt cache key to project binding on disk.
func SetCodexCursorPromptCacheProject(baseDir, promptCacheKey, projectID string) error {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	projectID = strings.TrimSpace(projectID)
	if promptCacheKey == "" || projectID == "" {
		return nil
	}
	codexCacheFileMu.Lock()
	defer codexCacheFileMu.Unlock()

	storePath, err := codexCursorPromptCacheProjectPath(baseDir)
	if err != nil {
		return err
	}
	store, err := loadCodexCursorPromptCacheProjectFile(storePath)
	if err != nil {
		return err
	}
	store.Entries[promptCacheKey] = CodexCursorPromptCacheProject{ProjectID: projectID, UpdatedAt: time.Now()}
	return saveCodexCursorPromptCacheProjectFile(storePath, store)
}

func codexCachePath(baseDir string) (string, error) {
	dir, err := codexCacheDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexCacheFileName), nil
}

func codexClaudeSessionProjectCachePath(baseDir string) (string, error) {
	dir, err := codexCacheDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexClaudeSessionProjectFileName), nil
}

func codexCursorPromptCacheProjectPath(baseDir string) (string, error) {
	dir, err := codexCacheDir(baseDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexCursorPromptCacheProjectFileName), nil
}

func codexCacheDir(baseDir string) (string, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return "", fmt.Errorf("codex prompt cache: base directory is empty")
	}
	return filepath.Join(baseDir, codexCacheDirName, codexPromptCacheDirName), nil
}

// MigrateCodexCacheFiles moves legacy Codex cache JSON files out of auth-dir/cache.
func MigrateCodexCacheFiles(baseDir, legacyBaseDir string) error {
	baseDir = strings.TrimSpace(baseDir)
	legacyBaseDir = strings.TrimSpace(legacyBaseDir)
	if baseDir == "" || legacyBaseDir == "" {
		return nil
	}
	targetDir, err := codexCacheDir(baseDir)
	if err != nil {
		return err
	}
	legacyDir := filepath.Join(legacyBaseDir, codexCacheDirName)
	if samePath(targetDir, legacyDir) {
		return nil
	}

	for _, name := range []string{codexCacheFileName, codexClaudeSessionProjectFileName, codexCursorPromptCacheProjectFileName} {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(targetDir, name)
		if _, err = os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat legacy codex cache %s: %w", name, err)
		}
		if _, err = os.Stat(dst); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat codex cache %s: %w", name, err)
		}
		if err = os.MkdirAll(targetDir, 0o700); err != nil {
			return fmt.Errorf("create codex cache dir: %w", err)
		}
		if err = os.Rename(src, dst); err != nil {
			if errCopy := copyFile(src, dst, 0o600); errCopy != nil {
				return fmt.Errorf("move codex cache %s: rename failed: %w; copy failed: %v", name, err, errCopy)
			}
			if errRemove := os.Remove(src); errRemove != nil {
				return fmt.Errorf("remove legacy codex cache %s after copy: %w", name, errRemove)
			}
		}
	}
	return nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if absA, err := filepath.Abs(a); err == nil {
		a = absA
	}
	if absB, err := filepath.Abs(b); err == nil {
		b = absB
	}
	return a == b
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
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

func loadCodexCursorPromptCacheProjectFile(path string) (codexCursorPromptCacheProjectFile, error) {
	store := codexCursorPromptCacheProjectFile{Entries: make(map[string]CodexCursorPromptCacheProject)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return store, fmt.Errorf("read codex Cursor prompt cache project cache: %w", err)
	}
	if len(data) == 0 {
		return store, nil
	}
	if err = json.Unmarshal(data, &store); err != nil {
		return codexCursorPromptCacheProjectFile{Entries: make(map[string]CodexCursorPromptCacheProject)}, fmt.Errorf("parse codex Cursor prompt cache project cache: %w", err)
	}
	if store.Entries == nil {
		store.Entries = make(map[string]CodexCursorPromptCacheProject)
	}
	return store, nil
}

func saveCodexCursorPromptCacheProjectFile(path string, store codexCursorPromptCacheProjectFile) error {
	if store.Entries == nil {
		store.Entries = make(map[string]CodexCursorPromptCacheProject)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create codex Cursor prompt cache project cache dir: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex Cursor prompt cache project cache: %w", err)
	}
	tmpPath := path + ".tmp"
	if err = os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write codex Cursor prompt cache project cache: %w", err)
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("remove old codex Cursor prompt cache project cache: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace codex Cursor prompt cache project cache: %w", err)
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
