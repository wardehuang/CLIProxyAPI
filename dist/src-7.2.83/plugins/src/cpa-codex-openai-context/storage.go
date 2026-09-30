package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type promptCacheEntry struct {
	ProjectID              string `json:"project_id,omitempty"`
	PromptCacheKey         string `json:"prompt_cache_key"`
	UpstreamPromptCacheKey string `json:"upstream_prompt_cache_key"`
	PromptCachedID         string `json:"prompt_cached_id"`
}

func loadOrCreatePromptCacheEntry(ctx context.Context, hostCallbackID, projectID, promptCacheKey string) (promptCacheEntry, error) {
	_ = ctx
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if promptCacheKey == "" {
		return promptCacheEntry{}, fmt.Errorf("prompt cache key is required")
	}
	key := promptCacheStorageKey(projectID, promptCacheKey)
	if entry, ok, errGet := getPromptCacheEntry(hostCallbackID, key); errGet != nil {
		return promptCacheEntry{}, errGet
	} else if ok {
		return entry, nil
	}
	entry := promptCacheEntryFor(projectID, promptCacheKey)
	if errSet := setPromptCacheEntry(hostCallbackID, key, entry); errSet != nil {
		return promptCacheEntry{}, errSet
	}
	return entry, nil
}

func promptCacheEntryFor(projectID, promptCacheKey string) promptCacheEntry {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	seed := firstNonEmpty(projectID, "global") + ":" + promptCacheKey
	upstreamKey := stableUUID("upstream:" + seed)
	return promptCacheEntry{
		ProjectID:              strings.TrimSpace(projectID),
		PromptCacheKey:         promptCacheKey,
		UpstreamPromptCacheKey: upstreamKey,
		PromptCachedID:         stableUUID("cached:" + seed),
	}
}

func promptCacheStorageKey(projectID, promptCacheKey string) string {
	projectScope := firstNonEmpty(projectID, "global")
	return "prompt-cache/" + sha1Hex(projectScope+":"+promptCacheKey) + ".json"
}

func getPromptCacheEntry(hostCallbackID, key string) (promptCacheEntry, bool, error) {
	result, errCall := callHost(pluginabi.MethodHostStorageGet, struct {
		pluginapi.HostStorageGetRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}{
		HostStorageGetRequest: pluginapi.HostStorageGetRequest{Key: key},
		HostCallbackID:        hostCallbackID,
	})
	if errCall != nil {
		return promptCacheEntry{}, false, errCall
	}
	var resp pluginapi.HostStorageGetResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return promptCacheEntry{}, false, fmt.Errorf("decode host storage get response: %w", errUnmarshal)
	}
	if !resp.Found || len(resp.Value) == 0 {
		return promptCacheEntry{}, false, nil
	}
	var entry promptCacheEntry
	if errUnmarshal := json.Unmarshal(resp.Value, &entry); errUnmarshal != nil {
		return promptCacheEntry{}, false, fmt.Errorf("decode prompt cache entry: %w", errUnmarshal)
	}
	if strings.TrimSpace(entry.UpstreamPromptCacheKey) == "" {
		return promptCacheEntry{}, false, nil
	}
	return entry, true, nil
}

func setPromptCacheEntry(hostCallbackID, key string, entry promptCacheEntry) error {
	value, errMarshal := json.Marshal(entry)
	if errMarshal != nil {
		return errMarshal
	}
	_, errCall := callHost(pluginabi.MethodHostStorageSet, struct {
		pluginapi.HostStorageSetRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}{
		HostStorageSetRequest: pluginapi.HostStorageSetRequest{Key: key, Value: value},
		HostCallbackID:        hostCallbackID,
	})
	return errCall
}
