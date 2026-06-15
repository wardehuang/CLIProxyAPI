package pluginhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type scopedStorage struct {
	mu sync.Mutex
}

func newScopedStorage() *scopedStorage {
	return &scopedStorage{}
}

func (s *scopedStorage) get(root, pluginID, key string) ([]byte, bool, error) {
	path, err := scopedStoragePath(root, pluginID, key)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil, false, nil
		}
		return nil, false, errRead
	}
	return raw, true, nil
}

func (s *scopedStorage) set(root, pluginID, key string, value []byte) error {
	path, err := scopedStoragePath(root, pluginID, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return errMkdir
	}
	tmp, errCreate := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if errCreate != nil {
		return errCreate
	}
	tmpName := tmp.Name()
	_, errWrite := tmp.Write(value)
	errClose := tmp.Close()
	if errWrite != nil || errClose != nil {
		_ = os.Remove(tmpName)
		if errWrite != nil {
			return errWrite
		}
		return errClose
	}
	return os.Rename(tmpName, path)
}

func (s *scopedStorage) delete(root, pluginID, key string) error {
	path, err := scopedStoragePath(root, pluginID, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	errRemove := os.Remove(path)
	if errRemove != nil && !os.IsNotExist(errRemove) {
		return errRemove
	}
	return nil
}

func (s *scopedStorage) list(root, pluginID, prefix string) ([]string, error) {
	pluginRoot, err := scopedStoragePluginRoot(root, pluginID)
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(prefix)), "./")
	if prefix == "." {
		prefix = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	errWalk := filepath.WalkDir(pluginRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry == nil || entry.IsDir() {
			return nil
		}
		rel, errRel := filepath.Rel(pluginRoot, path)
		if errRel != nil {
			return errRel
		}
		key := filepath.ToSlash(rel)
		if prefix == "" || strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if errWalk != nil && !os.IsNotExist(errWalk) {
		return nil, errWalk
	}
	return keys, nil
}

func scopedStoragePath(root, pluginID, key string) (string, error) {
	pluginRoot, err := scopedStoragePluginRoot(root, pluginID)
	if err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("storage key is empty")
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	path := filepath.Join(pluginRoot, clean)
	rel, errRel := filepath.Rel(pluginRoot, path)
	if errRel != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	return path, nil
}

func scopedStoragePluginRoot(root, pluginID string) (string, error) {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return "", fmt.Errorf("plugin id is required")
	}
	if root == "" {
		root = filepath.Join("plugins", "storage")
	}
	return filepath.Join(root, pluginID), nil
}

func (h *Host) storageRoot() string {
	if h == nil {
		return filepath.Join("plugins", "storage")
	}
	h.mu.Lock()
	cfg := h.runtimeConfig
	h.mu.Unlock()
	dir := "plugins"
	if cfg != nil && strings.TrimSpace(cfg.Plugins.Dir) != "" {
		dir = strings.TrimSpace(cfg.Plugins.Dir)
	}
	return filepath.Join(dir, "storage")
}
