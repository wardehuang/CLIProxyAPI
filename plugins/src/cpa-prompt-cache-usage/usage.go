package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var usageStatsMu sync.Mutex

const (
	metadataProjectID              = "project_id"
	metadataPromptCacheKey         = "prompt_cache_key"
	metadataUpstreamPromptCacheKey = "upstream_prompt_cache_key"
	metadataPromptCachedID         = "prompt_cached_id"

	metadataCPAProjectID              = "cpa.project_id"
	metadataCPAPromptCacheKey         = "cpa.prompt_cache_key"
	metadataCPAUpstreamPromptCacheKey = "cpa.upstream_prompt_cache_key"
	metadataCPAPromptCachedID         = "cpa.prompt_cached_id"
)

type usageContextInfo struct {
	ProjectID              string `json:"project_id,omitempty"`
	PromptCacheKey         string `json:"prompt_cache_key,omitempty"`
	UpstreamPromptCacheKey string `json:"upstream_prompt_cache_key,omitempty"`
	PromptCachedID         string `json:"prompt_cached_id,omitempty"`
}

type promptCacheUsageStats struct {
	ProjectID              string           `json:"project_id,omitempty"`
	PromptCacheKey         string           `json:"prompt_cache_key"`
	UpstreamPromptCacheKey string           `json:"upstream_prompt_cache_key,omitempty"`
	PromptCachedID         string           `json:"prompt_cached_id,omitempty"`
	Requests               int64            `json:"requests"`
	FailedRequests         int64            `json:"failed_requests,omitempty"`
	InputTokens            int64            `json:"input_tokens,omitempty"`
	OutputTokens           int64            `json:"output_tokens,omitempty"`
	ReasoningTokens        int64            `json:"reasoning_tokens,omitempty"`
	CachedTokens           int64            `json:"cached_tokens,omitempty"`
	CacheReadTokens        int64            `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens    int64            `json:"cache_creation_tokens,omitempty"`
	TotalTokens            int64            `json:"total_tokens,omitempty"`
	Models                 map[string]int64 `json:"models,omitempty"`
	Aliases                map[string]int64 `json:"aliases,omitempty"`
	Sources                map[string]int64 `json:"sources,omitempty"`
	FirstSeenAt            time.Time        `json:"first_seen_at"`
	LastSeenAt             time.Time        `json:"last_seen_at"`
}

type projectUsageStats struct {
	ProjectID           string           `json:"project_id"`
	Requests            int64            `json:"requests"`
	FailedRequests      int64            `json:"failed_requests,omitempty"`
	PromptCacheKeys     map[string]int64 `json:"prompt_cache_keys,omitempty"`
	PromptCachedIDs     map[string]int64 `json:"prompt_cached_ids,omitempty"`
	InputTokens         int64            `json:"input_tokens,omitempty"`
	OutputTokens        int64            `json:"output_tokens,omitempty"`
	ReasoningTokens     int64            `json:"reasoning_tokens,omitempty"`
	CachedTokens        int64            `json:"cached_tokens,omitempty"`
	CacheReadTokens     int64            `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64            `json:"cache_creation_tokens,omitempty"`
	TotalTokens         int64            `json:"total_tokens,omitempty"`
	FirstSeenAt         time.Time        `json:"first_seen_at"`
	LastSeenAt          time.Time        `json:"last_seen_at"`
}

func handleUsage(ctx context.Context, record pluginapi.UsageRecord) error {
	_ = ctx
	info, ok := usageContextInfoFromMetadata(record.Metadata)
	if !ok {
		return nil
	}
	usageStatsMu.Lock()
	defer usageStatsMu.Unlock()
	now := record.RequestedAt
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if errUpdate := updatePromptCacheUsageStats(info, record, now); errUpdate != nil {
		return errUpdate
	}
	if strings.TrimSpace(info.ProjectID) == "" {
		return nil
	}
	return updateProjectUsageStats(info, record, now)
}

func usageContextInfoFromMetadata(metadata map[string]any) (usageContextInfo, bool) {
	info := usageContextInfo{
		ProjectID:              firstNonEmpty(stringFromMetadata(metadata, metadataCPAProjectID), stringFromMetadata(metadata, metadataProjectID)),
		PromptCacheKey:         firstNonEmpty(stringFromMetadata(metadata, metadataCPAPromptCacheKey), stringFromMetadata(metadata, metadataPromptCacheKey)),
		UpstreamPromptCacheKey: firstNonEmpty(stringFromMetadata(metadata, metadataCPAUpstreamPromptCacheKey), stringFromMetadata(metadata, metadataUpstreamPromptCacheKey)),
		PromptCachedID:         firstNonEmpty(stringFromMetadata(metadata, metadataCPAPromptCachedID), stringFromMetadata(metadata, metadataPromptCachedID)),
	}
	return info, info.PromptCacheKey != "" || info.UpstreamPromptCacheKey != "" || info.PromptCachedID != ""
}

func updatePromptCacheUsageStats(info usageContextInfo, record pluginapi.UsageRecord, now time.Time) error {
	storageKey := promptCacheUsageStorageKey(info)
	stats := promptCacheUsageStats{}
	if ok, errLoad := loadStorageJSON(storageKey, &stats); errLoad != nil {
		return errLoad
	} else if !ok {
		stats = promptCacheUsageStats{
			ProjectID:              info.ProjectID,
			PromptCacheKey:         info.PromptCacheKey,
			UpstreamPromptCacheKey: info.UpstreamPromptCacheKey,
			PromptCachedID:         info.PromptCachedID,
			Models:                 map[string]int64{},
			Aliases:                map[string]int64{},
			Sources:                map[string]int64{},
			FirstSeenAt:            now,
		}
	}
	stats.ProjectID = firstNonEmpty(stats.ProjectID, info.ProjectID)
	stats.PromptCacheKey = firstNonEmpty(stats.PromptCacheKey, info.PromptCacheKey)
	stats.UpstreamPromptCacheKey = firstNonEmpty(stats.UpstreamPromptCacheKey, info.UpstreamPromptCacheKey)
	stats.PromptCachedID = firstNonEmpty(stats.PromptCachedID, info.PromptCachedID)
	stats.Requests++
	if record.Failed {
		stats.FailedRequests++
	}
	addUsageDetailToPromptStats(&stats, record.Detail)
	incrementStringCounter(&stats.Models, record.Model)
	incrementStringCounter(&stats.Aliases, record.Alias)
	incrementStringCounter(&stats.Sources, record.Source)
	if stats.FirstSeenAt.IsZero() {
		stats.FirstSeenAt = now
	}
	stats.LastSeenAt = now
	return saveStorageJSON(storageKey, stats)
}

func updateProjectUsageStats(info usageContextInfo, record pluginapi.UsageRecord, now time.Time) error {
	storageKey := projectUsageStorageKey(info.ProjectID)
	stats := projectUsageStats{}
	if ok, errLoad := loadStorageJSON(storageKey, &stats); errLoad != nil {
		return errLoad
	} else if !ok {
		stats = projectUsageStats{
			ProjectID:       info.ProjectID,
			PromptCacheKeys: map[string]int64{},
			PromptCachedIDs: map[string]int64{},
			FirstSeenAt:     now,
		}
	}
	stats.ProjectID = firstNonEmpty(stats.ProjectID, info.ProjectID)
	stats.Requests++
	if record.Failed {
		stats.FailedRequests++
	}
	addUsageDetailToProjectStats(&stats, record.Detail)
	incrementStringCounter(&stats.PromptCacheKeys, info.PromptCacheKey)
	incrementStringCounter(&stats.PromptCachedIDs, info.PromptCachedID)
	if stats.FirstSeenAt.IsZero() {
		stats.FirstSeenAt = now
	}
	stats.LastSeenAt = now
	return saveStorageJSON(storageKey, stats)
}

func addUsageDetailToPromptStats(stats *promptCacheUsageStats, detail pluginapi.UsageDetail) {
	stats.InputTokens += detail.InputTokens
	stats.OutputTokens += detail.OutputTokens
	stats.ReasoningTokens += detail.ReasoningTokens
	stats.CachedTokens += detail.CachedTokens
	stats.CacheReadTokens += detail.CacheReadTokens
	stats.CacheCreationTokens += detail.CacheCreationTokens
	stats.TotalTokens += detail.TotalTokens
}

func addUsageDetailToProjectStats(stats *projectUsageStats, detail pluginapi.UsageDetail) {
	stats.InputTokens += detail.InputTokens
	stats.OutputTokens += detail.OutputTokens
	stats.ReasoningTokens += detail.ReasoningTokens
	stats.CachedTokens += detail.CachedTokens
	stats.CacheReadTokens += detail.CacheReadTokens
	stats.CacheCreationTokens += detail.CacheCreationTokens
	stats.TotalTokens += detail.TotalTokens
}

func loadStorageJSON(key string, target any) (bool, error) {
	result, errCall := callHost(pluginabi.MethodHostStorageGet, pluginapi.HostStorageGetRequest{Key: key})
	if errCall != nil {
		return false, errCall
	}
	var resp pluginapi.HostStorageGetResponse
	if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
		return false, fmt.Errorf("decode host storage get response: %w", errUnmarshal)
	}
	if !resp.Found || len(resp.Value) == 0 {
		return false, nil
	}
	if errUnmarshal := json.Unmarshal(resp.Value, target); errUnmarshal != nil {
		return false, fmt.Errorf("decode storage value %s: %w", key, errUnmarshal)
	}
	return true, nil
}

func saveStorageJSON(key string, value any) error {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return errMarshal
	}
	_, errCall := callHost(pluginabi.MethodHostStorageSet, pluginapi.HostStorageSetRequest{Key: key, Value: raw})
	return errCall
}

func promptCacheUsageStorageKey(info usageContextInfo) string {
	seed := firstNonEmpty(info.PromptCachedID, promptScopedSeed(info.ProjectID, info.PromptCacheKey), info.UpstreamPromptCacheKey)
	return "usage/prompt-cache/" + sha1Hex(seed) + ".json"
}

func promptScopedSeed(projectID, promptCacheKey string) string {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if promptCacheKey == "" {
		return ""
	}
	return firstNonEmpty(projectID, "global") + ":" + promptCacheKey
}

func projectUsageStorageKey(projectID string) string {
	return "usage/projects/" + sha1Hex(projectID) + ".json"
}

func incrementStringCounter(counters *map[string]int64, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if *counters == nil {
		*counters = map[string]int64{}
	}
	(*counters)[value]++
}

func stringFromMetadata(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func sha1Hex(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}
