package pluginhost

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Host) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	if h == nil {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	for _, record := range h.activeRecords() {
		if h.isPluginFused(record.id) || record.plugin.Capabilities.Scheduler == nil {
			continue
		}
		resp, invoked, errPick := h.callScheduler(ctx, record, req)
		if errPick != nil {
			return resp, true, errPick
		}
		if !invoked || !resp.Handled {
			continue
		}
		resp, valid, reason := normalizeSchedulerResponse(resp, req)
		if !valid {
			log.WithField("plugin_id", record.id).Warnf("pluginhost: scheduler returned invalid response: %s", reason)
			continue
		}
		return resp, true, nil
	}
	return pluginapi.SchedulerPickResponse{}, false, nil
}

func (h *Host) HasScheduler() bool {
	if h == nil {
		return false
	}
	for _, record := range h.activeRecords() {
		if !h.isPluginFused(record.id) && record.plugin.Capabilities.Scheduler != nil {
			return true
		}
	}
	return false
}

func (h *Host) callScheduler(ctx context.Context, record capabilityRecord, req pluginapi.SchedulerPickRequest) (resp pluginapi.SchedulerPickResponse, handled bool, err error) {
	scheduler := record.plugin.Capabilities.Scheduler
	if h == nil || scheduler == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "Scheduler.Pick", recovered)
			resp = pluginapi.SchedulerPickResponse{}
			handled = false
			err = nil
		}
	}()

	req.Plugin = record.meta
	resp, errPick := scheduler.Pick(ctx, req)
	if errPick != nil {
		log.WithField("plugin_id", record.id).WithError(errPick).Warn("pluginhost: scheduler rejected auth pick")
		return pluginapi.SchedulerPickResponse{}, true, errPick
	}
	return resp, true, nil
}

func normalizeSchedulerResponse(resp pluginapi.SchedulerPickResponse, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, string) {
	resp.AuthID = strings.TrimSpace(resp.AuthID)
	resp.DelegateBuiltin = strings.TrimSpace(resp.DelegateBuiltin)

	hasAuthID := resp.AuthID != ""
	hasDelegate := resp.DelegateBuiltin != ""
	hasRejection := resp.Rejection != nil
	decisionCount := 0
	if hasAuthID {
		decisionCount++
	}
	if hasDelegate {
		decisionCount++
	}
	if hasRejection {
		decisionCount++
	}
	if decisionCount != 1 {
		return pluginapi.SchedulerPickResponse{}, false, "scheduler response must contain exactly one decision"
	}
	if hasAuthID {
		if !schedulerCandidateExists(req.Candidates, resp.AuthID) && !schedulerCandidateExists(req.AllCandidates, resp.AuthID) {
			return pluginapi.SchedulerPickResponse{}, false, "unknown auth id"
		}
		return resp, true, ""
	}
	if hasRejection {
		resp.Rejection.Code = strings.TrimSpace(resp.Rejection.Code)
		resp.Rejection.Message = strings.TrimSpace(resp.Rejection.Message)
		if resp.Rejection.Code == "" || resp.Rejection.Message == "" {
			return pluginapi.SchedulerPickResponse{}, false, "rejection code and message are required"
		}
		if resp.Rejection.HTTPStatus < 100 || resp.Rejection.HTTPStatus > 599 {
			return pluginapi.SchedulerPickResponse{}, false, "invalid rejection http status"
		}
		return resp, true, ""
	}
	if !validSchedulerBuiltin(resp.DelegateBuiltin) {
		return pluginapi.SchedulerPickResponse{}, false, "unknown delegate"
	}
	return resp, true, ""
}

func schedulerCandidateExists(candidates []pluginapi.SchedulerAuthCandidate, authID string) bool {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == authID {
			return true
		}
	}
	return false
}

func validSchedulerBuiltin(delegate string) bool {
	switch delegate {
	case pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst:
		return true
	default:
		return false
	}
}
