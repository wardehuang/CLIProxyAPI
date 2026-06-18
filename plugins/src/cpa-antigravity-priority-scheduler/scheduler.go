package main

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerAntigravity = "antigravity"
	modelFamilyClaude   = "claude"
	modelFamilyGemini   = "gemini"
	attrPriorityClaude  = "priority_claude"
	attrPriorityGemini  = "priority_gemini"
)

var schedulerState = struct {
	sync.Mutex
	cursors map[string]int
}{cursors: make(map[string]int)}

type prioritizedCandidate struct {
	candidate pluginapi.SchedulerAuthCandidate
	priority  int
}

func pickAntigravityAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	_ = ctx
	family := antigravityModelFamily(req.Model)
	if family == "" || !requestIncludesAntigravity(req) {
		return pluginapi.SchedulerPickResponse{}
	}

	attrName := attrPriorityGemini
	if family == modelFamilyClaude {
		attrName = attrPriorityClaude
	}

	candidates := make([]prioritizedCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), providerAntigravity) {
			continue
		}
		priority, ok := parseCandidatePriority(candidate.Attributes, attrName)
		if !ok {
			logPluginDebug("candidate missing family priority", map[string]any{
				"auth_id": candidate.ID,
				"family":  family,
				"attr":    attrName,
			})
			return pluginapi.SchedulerPickResponse{}
		}
		candidates = append(candidates, prioritizedCandidate{candidate: candidate, priority: priority})
	}
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].candidate.ID < candidates[j].candidate.ID
	})

	config := currentPluginConfig()
	selected := candidates[0]
	if config.Strategy == strategyRoundRobin {
		selected = pickRoundRobin(req, family, candidates)
	}

	logPluginDebug("selected antigravity auth", map[string]any{
		"auth_id":  selected.candidate.ID,
		"family":   family,
		"priority": selected.priority,
		"strategy": config.Strategy,
		"model":    req.Model,
	})
	return pluginapi.SchedulerPickResponse{AuthID: selected.candidate.ID, Handled: true}
}

func requestIncludesAntigravity(req pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), providerAntigravity) {
		return true
	}
	for _, provider := range req.Providers {
		if strings.EqualFold(strings.TrimSpace(provider), providerAntigravity) {
			return true
		}
	}
	return false
}

func antigravityModelFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(model, modelFamilyClaude):
		return modelFamilyClaude
	case strings.Contains(model, modelFamilyGemini):
		return modelFamilyGemini
	default:
		return ""
	}
}

func parseCandidatePriority(attrs map[string]string, key string) (int, bool) {
	if len(attrs) == 0 {
		return 0, false
	}
	raw := strings.TrimSpace(attrs[key])
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func pickRoundRobin(req pluginapi.SchedulerPickRequest, family string, candidates []prioritizedCandidate) prioritizedCandidate {
	bestPriority := candidates[0].priority
	top := make([]prioritizedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.priority != bestPriority {
			break
		}
		top = append(top, candidate)
	}
	if len(top) <= 1 {
		return top[0]
	}

	key := schedulerCursorKey(req, family, bestPriority)
	schedulerState.Lock()
	idx := schedulerState.cursors[key]
	if idx >= 2_147_483_640 {
		idx = 0
	}
	schedulerState.cursors[key] = idx + 1
	schedulerState.Unlock()
	return top[idx%len(top)]
}

func schedulerCursorKey(req pluginapi.SchedulerPickRequest, family string, priority int) string {
	model := strings.ToLower(strings.TrimSpace(req.Model))
	if model == "" {
		model = "unknown"
	}
	return providerAntigravity + ":" + family + ":" + model + ":" + strconv.Itoa(priority) + ":" + hashCandidateIDs(req.Candidates)
}

func hashCandidateIDs(candidates []pluginapi.SchedulerAuthCandidate) string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.Provider), providerAntigravity) {
			ids = append(ids, strings.TrimSpace(candidate.ID))
		}
	}
	sort.Strings(ids)
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.Join(ids, ",")))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}
