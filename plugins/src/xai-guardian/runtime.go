package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

var guardianRuntime = &runtimeController{scheduleGroups: newScheduleGroupState()}

type inspectionScheduleConfig struct {
	Enabled         bool
	Mode            string
	IntervalSeconds int
	DailyTime       string
}

type inspectionScheduleStatus struct {
	Enabled         bool   `json:"enabled"`
	Mode            string `json:"mode"`
	IntervalSeconds int    `json:"intervalSeconds"`
	DailyTime       string `json:"dailyTime"`
	NextRunAtMS     int64  `json:"nextRunAtMs"`
}

type inspectionScheduleClock struct {
	mutex   sync.RWMutex
	nextRun time.Time
}

func (clock *inspectionScheduleClock) setNextRun(nextRun time.Time) {
	clock.mutex.Lock()
	clock.nextRun = nextRun
	clock.mutex.Unlock()
}

func (clock *inspectionScheduleClock) nextRunAt() time.Time {
	clock.mutex.RLock()
	defer clock.mutex.RUnlock()
	return clock.nextRun
}

func (controller *runtimeController) currentInspectionScheduleStatus(settings pluginSettings) (inspectionScheduleStatus, error) {
	schedule := inspectionScheduleFromSettings(settings)
	status := inspectionScheduleStatus{
		Enabled:         schedule.Enabled,
		Mode:            schedule.Mode,
		IntervalSeconds: schedule.IntervalSeconds,
		DailyTime:       schedule.DailyTime,
	}
	if !schedule.Enabled {
		return status, nil
	}
	nextRun := controller.inspectionScheduleClock.nextRunAt()
	if nextRun.IsZero() {
		switch schedule.Mode {
		case inspectionScheduleModeInterval:
			nextRun = time.Now().Add(time.Duration(schedule.IntervalSeconds) * time.Second)
		case inspectionScheduleModeDailyTime:
			var err error
			nextRun, err = nextInspectionDailyTime(time.Now(), schedule.DailyTime)
			if err != nil {
				return inspectionScheduleStatus{}, err
			}
		default:
			return inspectionScheduleStatus{}, fmt.Errorf("unsupported inspection schedule mode %q", schedule.Mode)
		}
	}
	status.NextRunAtMS = nextRun.UnixMilli()
	return status, nil
}

func inspectionScheduleFromSettings(settings pluginSettings) inspectionScheduleConfig {
	return inspectionScheduleConfig{
		Enabled:         settings.InspectionScheduleEnabled,
		Mode:            settings.InspectionScheduleMode,
		IntervalSeconds: settings.InspectionIntervalSeconds,
		DailyTime:       settings.InspectionDailyTime,
	}
}

type runtimeController struct {
	mutex                     sync.RWMutex
	store                     *guardianStore
	refreshProxyBatchMutex    sync.RWMutex
	refreshProxyBatchJob      *authRefreshProxyBatchJob
	refreshProxyBatchCancel   context.CancelFunc
	refreshProxyBatchGroup    sync.WaitGroup
	inspectionCancel          context.CancelFunc
	inspectionScheduleUpdates chan inspectionScheduleConfig
	inspectionScheduleClock   inspectionScheduleClock
	manualInspectionCancel    context.CancelFunc
	inspectionGroup           sync.WaitGroup
	probeCancel               context.CancelFunc
	probeGroup                sync.WaitGroup
	probeTrigger              chan struct{}
	keepaliveCancel           context.CancelFunc
	keepaliveGroup            sync.WaitGroup
	reviveCancel              context.CancelFunc
	reviveGroup               sync.WaitGroup
	keepaliveState            *keepaliveScheduleState
	scheduleGroups            *scheduleGroupState
	runtimeScheduleGroupCount int
	config                    pluginConfig
}

type authRefreshProxyBatchJob struct {
	ID               string
	Status           string
	AuthCount        int
	ProxyCount       int
	ProcessedCount   int
	UpdatedCount     int
	FailedCount      int
	FailedAuthNames  []string
	UnusedProxyCount int
	Error            string
}

func (controller *runtimeController) configure(config pluginConfig) error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store == nil {
		store, err := openGuardianStore(config.DatabasePath)
		if err != nil {
			return err
		}
		controller.store = store
	} else {
		configuredPath, err := normalizedDatabasePath(config.DatabasePath)
		if err != nil {
			return err
		}
		if controller.store.path != configuredPath {
			return fmt.Errorf("database path changes require plugin restart")
		}
	}
	settings, err := controller.store.settings()
	if err != nil {
		return err
	}
	if err := controller.store.setSettings(settings); err != nil {
		return err
	}
	controller.stopWorkersLocked()
	if err := controller.store.reconcileHealthySlots(settings); err != nil {
		return err
	}
	if err := refreshHealthyAuthDistribution(controller.store); err != nil {
		_ = controller.store.appendLog(logLevelError, "auth.distribution_failed", "健康槽位 auth 分配失败", sanitizeLogText(err.Error()))
	}
	if err := controller.store.reconcileScheduleGroupCounters(settings.ScheduleGroupCount); err != nil {
		return err
	}
	controller.runtimeScheduleGroupCount = settings.ScheduleGroupCount
	controller.config = config
	controller.scheduleGroups.resetRuntime()
	if settings.InspectionScheduleEnabled {
		controller.startInspectionWorkerLocked(inspectionScheduleFromSettings(settings))
	}
	controller.startInitialProbeWorkerLocked(settings)
	if settings.KeepaliveIntervalSeconds > 0 {
		controller.startKeepaliveWorkerLocked(settings)
	}
	if settings.ReviveIntervalSeconds > 0 {
		controller.startReviveWorkerLocked(settings)
	}
	return controller.store.appendLog(logLevelInfo, "plugin.configured", "xAI Guardian 已初始化", fmt.Sprintf("数据库 %s", controller.store.path))
}

func (controller *runtimeController) ensure() error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store != nil {
		return nil
	}
	store, err := openGuardianStore(defaultDatabasePath)
	if err != nil {
		return err
	}
	controller.store = store
	settings, err := store.settings()
	if err != nil {
		_ = store.close()
		controller.store = nil
		return err
	}
	if err := store.reconcileScheduleGroupCounters(settings.ScheduleGroupCount); err != nil {
		_ = store.close()
		controller.store = nil
		return err
	}
	if err := store.reconcileHealthySlots(settings); err != nil {
		_ = store.close()
		controller.store = nil
		return err
	}
	if err := refreshHealthyAuthDistribution(store); err != nil {
		_ = store.appendLog(logLevelError, "auth.distribution_failed", "健康槽位 auth 分配失败", sanitizeLogText(err.Error()))
	}
	controller.runtimeScheduleGroupCount = settings.ScheduleGroupCount
	if settings.InspectionScheduleEnabled {
		controller.startInspectionWorkerLocked(inspectionScheduleFromSettings(settings))
	}
	controller.startInitialProbeWorkerLocked(settings)
	if settings.KeepaliveIntervalSeconds > 0 {
		controller.startKeepaliveWorkerLocked(settings)
	}
	if settings.ReviveIntervalSeconds > 0 {
		controller.startReviveWorkerLocked(settings)
	}
	return nil
}

func (controller *runtimeController) currentStore() *guardianStore {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	return controller.store
}

func (controller *runtimeController) stopWorkersLocked() {
	if controller.inspectionCancel != nil {
		controller.inspectionCancel()
		controller.inspectionCancel = nil
	}
	if controller.manualInspectionCancel != nil {
		controller.manualInspectionCancel()
		controller.manualInspectionCancel = nil
	}
	controller.inspectionGroup.Wait()
	controller.inspectionScheduleUpdates = nil
	controller.inspectionScheduleClock.setNextRun(time.Time{})
	if controller.probeCancel != nil {
		controller.probeCancel()
		controller.probeGroup.Wait()
		controller.probeCancel = nil
		controller.probeTrigger = nil
	}
	if controller.keepaliveCancel != nil {
		controller.keepaliveCancel()
		controller.keepaliveGroup.Wait()
		controller.keepaliveCancel = nil
	}
	if controller.reviveCancel != nil {
		controller.reviveCancel()
		controller.reviveGroup.Wait()
		controller.reviveCancel = nil
	}
	controller.keepaliveState = nil
}

func (controller *runtimeController) startInspectionWorkerLocked(schedule inspectionScheduleConfig) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.inspectionCancel = cancel
	controller.inspectionScheduleUpdates = make(chan inspectionScheduleConfig, 1)
	scheduleUpdates := controller.inspectionScheduleUpdates
	store := controller.store
	scheduleClock := &controller.inspectionScheduleClock
	scheduleClock.setNextRun(time.Time{})
	controller.inspectionGroup.Add(1)
	go func() {
		defer controller.inspectionGroup.Done()
		var ticker *time.Ticker
		var timer *time.Timer
		var scheduleChannel <-chan time.Time
		stopSchedule := func() {
			if ticker != nil {
				ticker.Stop()
				ticker = nil
			}
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			scheduleChannel = nil
		}
		resetSchedule := func(next inspectionScheduleConfig) error {
			stopSchedule()
			scheduleClock.setNextRun(time.Time{})
			if !next.Enabled {
				return nil
			}
			switch next.Mode {
			case inspectionScheduleModeInterval:
				interval := time.Duration(next.IntervalSeconds) * time.Second
				ticker = time.NewTicker(interval)
				scheduleClock.setNextRun(time.Now().Add(interval))
				scheduleChannel = ticker.C
			case inspectionScheduleModeDailyTime:
				nextRun, err := nextInspectionDailyTime(time.Now(), next.DailyTime)
				if err != nil {
					return err
				}
				scheduleClock.setNextRun(nextRun)
				timer = time.NewTimer(time.Until(nextRun))
				scheduleChannel = timer.C
			default:
				return fmt.Errorf("unsupported inspection schedule mode %q", next.Mode)
			}
			return nil
		}
		defer stopSchedule()
		if err := resetSchedule(schedule); err != nil {
			_ = store.appendLog(logLevelError, "inspection.schedule_failed", "自动服务端巡检调度配置无效", sanitizeLogText(err.Error()))
			return
		}
		for {
			select {
			case <-workerContext.Done():
				return
			case next := <-scheduleUpdates:
				schedule = next
				if err := resetSchedule(schedule); err != nil {
					_ = store.appendLog(logLevelError, "inspection.schedule_failed", "自动服务端巡检调度配置无效", sanitizeLogText(err.Error()))
					return
				}
			case scheduledAt := <-scheduleChannel:
				select {
				case next := <-scheduleUpdates:
					schedule = next
					if err := resetSchedule(schedule); err != nil {
						_ = store.appendLog(logLevelError, "inspection.schedule_failed", "自动服务端巡检调度配置无效", sanitizeLogText(err.Error()))
						return
					}
					continue
				default:
				}
				switch schedule.Mode {
				case inspectionScheduleModeInterval:
					scheduleClock.setNextRun(scheduledAt.Add(time.Duration(schedule.IntervalSeconds) * time.Second))
				case inspectionScheduleModeDailyTime:
					nextRun, err := nextInspectionDailyTime(time.Now(), schedule.DailyTime)
					if err != nil {
						_ = store.appendLog(logLevelError, "inspection.schedule_failed", "自动服务端巡检调度配置无效", sanitizeLogText(err.Error()))
						return
					}
					scheduleClock.setNextRun(nextRun)
				}
				if err := runInspection(workerContext, store); err != nil {
					_ = store.appendLog(logLevelError, "inspection.worker_failed", "自动服务端巡检失败", sanitizeLogText(err.Error()))
				}
				if schedule.Enabled && schedule.Mode == inspectionScheduleModeDailyTime {
					if err := resetSchedule(schedule); err != nil {
						_ = store.appendLog(logLevelError, "inspection.schedule_failed", "自动服务端巡检调度配置无效", sanitizeLogText(err.Error()))
						return
					}
				}
			}
		}
	}()
}

func nextInspectionDailyTime(now time.Time, dailyTime string) (time.Time, error) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Time{}, fmt.Errorf("load Beijing time zone: %w", err)
	}
	parsed, err := time.Parse("15:04", dailyTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse inspection daily time: %w", err)
	}
	localNow := now.In(location)
	nextRun := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), parsed.Hour(), parsed.Minute(), 0, 0, location)
	if !nextRun.After(localNow) {
		nextRun = nextRun.AddDate(0, 0, 1)
	}
	return nextRun, nil
}

func (controller *runtimeController) setInspectionScheduleLocked(schedule inspectionScheduleConfig) {
	controller.inspectionScheduleClock.setNextRun(time.Time{})
	if controller.inspectionCancel == nil {
		if schedule.Enabled {
			controller.startInspectionWorkerLocked(schedule)
		}
		return
	}
	updates := controller.inspectionScheduleUpdates
	select {
	case updates <- schedule:
	default:
		select {
		case <-updates:
		default:
		}
		updates <- schedule
	}
}

func (controller *runtimeController) startKeepaliveWorkerLocked(settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.keepaliveCancel = cancel
	controller.keepaliveState = newKeepaliveScheduleState()
	store := controller.store
	state := controller.keepaliveState
	controller.keepaliveGroup.Add(1)
	go func() {
		defer controller.keepaliveGroup.Done()
		runKeepaliveScheduler(workerContext, store, state, settings)
	}()
}

func (controller *runtimeController) startInitialProbeWorkerLocked(settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.probeCancel = cancel
	controller.probeTrigger = make(chan struct{}, 1)
	store := controller.store
	trigger := controller.probeTrigger
	controller.probeGroup.Add(1)
	go func() {
		defer controller.probeGroup.Done()
		runInitialProbeScheduler(workerContext, store, settings, trigger)
	}()
}

func (controller *runtimeController) startReviveWorkerLocked(settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.reviveCancel = cancel
	store := controller.store
	controller.reviveGroup.Add(1)
	go func() {
		defer controller.reviveGroup.Done()
		runReviveScheduler(workerContext, store, settings)
	}()
}

func (controller *runtimeController) shutdown() {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.refreshProxyBatchCancel != nil {
		controller.refreshProxyBatchCancel()
	}
	controller.stopWorkersLocked()
	controller.refreshProxyBatchGroup.Wait()
	controller.refreshProxyBatchCancel = nil
	controller.scheduleGroups.resetRuntime()
	if controller.store != nil {
		_ = controller.store.appendLog(logLevelInfo, "plugin.shutdown", "xAI Guardian 正在停止", "")
		_ = controller.store.close()
		controller.store = nil
	}
}

func (controller *runtimeController) api(method, path string, query url.Values, body []byte) (int, []byte, error) {
	if err := controller.ensure(); err != nil {
		return http.StatusInternalServerError, nil, err
	}
	controller.mutex.RLock()
	store := controller.store
	controller.mutex.RUnlock()
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	if method == http.MethodGet && path == "/api/summary" {
		value, err := store.summary()
		return jsonAPIResult(value, err)
	}
	if method == http.MethodGet && path == "/api/schedule-groups/counters" {
		return controller.scheduleGroupCountersAPI(store)
	}
	if method == http.MethodGet && path == "/api/settings" {
		settings, err := store.settings()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(publicSettings(settings), nil)
	}
	if (method == http.MethodPut || method == http.MethodPost) && path == "/api/settings" {
		return controller.updateSettings(store, body)
	}
	if method == http.MethodGet && path == "/api/inspection/settings" {
		return controller.inspectionSettingsAPI(store)
	}
	if method == http.MethodPut && path == "/api/inspection/settings" {
		return controller.updateInspectionSettings(store, body)
	}
	if method == http.MethodGet && path == "/api/auths/refresh-proxy-urls/status" {
		return controller.authRefreshProxyBatchStatusAPI(query.Get("jobId"))
	}
	if method == http.MethodPost && path == "/api/auths/refresh-proxy-urls" {
		return controller.startAuthRefreshProxyBatch(store, body)
	}
	if method == http.MethodGet && path == "/api/accounts" {
		return controller.accountsAPI(store)
	}
	if method == http.MethodPost && path == "/api/accounts/refresh" {
		return controller.refreshAccountsAPI(store)
	}
	if method == http.MethodPost && path == "/api/accounts/degradation-check" {
		return accountDegradationProbeAPI(store, body)
	}
	if method == http.MethodGet && path == "/api/accounts/degradation-check/latest" {
		return latestAccountDegradationProbeAPI(store, query.Get("authIndex"))
	}
	if method == http.MethodGet && path == "/api/batch-nodes" {
		nodes, err := store.listIPNodes()
		return jsonAPIResult(publicNodes(nodes), err)
	}
	if method == http.MethodGet && path == "/api/batches" {
		batches, err := store.listIPBatches()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		settings, err := store.settings()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(map[string]any{"items": publicIPBatches(batches, settings.IPBatchRetentionDays), "total": len(batches), "max": maxIPBatches, "retentionDays": settings.IPBatchRetentionDays}, nil)
	}
	if method == http.MethodPost && path == "/api/batches" {
		return controller.addIPBatch(store, body)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[1] == "batches" && parts[3] == "nodes" {
		batchID := strings.TrimSpace(parts[2])
		if batchID == "" {
			return http.StatusBadRequest, nil, fmt.Errorf("batch ID is required")
		}
		nodes, err := store.listIPBatchNodes(batchID)
		return jsonAPIResult(map[string]any{"batchId": batchID, "items": publicNodes(nodes), "total": len(nodes)}, err)
	}
	if method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[1] == "nodes" && parts[3] == "auth-bindings" {
		nodeID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || nodeID <= 0 {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid node id")
		}
		bindings, err := store.listAuthBindingsByNode(nodeID)
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		verifiedCount := 0
		syncFailureCount := 0
		for _, binding := range bindings {
			if binding.Status == "write_failed" {
				syncFailureCount++
			} else {
				verifiedCount++
			}
		}
		return jsonAPIResult(map[string]any{
			"nodeId":           nodeID,
			"items":            publicAuthBindings(bindings),
			"total":            len(bindings),
			"verifiedCount":    verifiedCount,
			"syncFailureCount": syncFailureCount,
		}, nil)
	}
	if method == http.MethodGet && path == "/api/inspection/runs" {
		limit, _ := strconv.Atoi(query.Get("limit"))
		runs, err := store.listAccountInspectionRuns(limit)
		return jsonAPIResult(runs, err)
	}
	if method == http.MethodGet && path == "/api/inspection" {
		return controller.inspectionAPI(store, query)
	}
	if method == http.MethodPost && path == "/api/inspection" {
		return controller.startAccountInspectionAPI(store)
	}
	if method == http.MethodGet && path == "/api/keepalive" {
		return controller.keepaliveAPI(store)
	}
	if method == http.MethodPost && path == "/api/keepalive/run" {
		return controller.runKeepaliveNow()
	}
	if method == http.MethodGet && path == "/api/degradation" {
		states, err := store.listDegradations()
		public := make([]map[string]any, 0, len(states))
		for _, state := range states {
			public = append(public, publicDegradation(state))
		}
		return jsonAPIResult(public, err)
	}
	if method == http.MethodPost && path == "/api/degradation/clear" {
		var payload struct {
			AuthIndex string `json:"authIndex"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.AuthIndex) == "" {
			return http.StatusBadRequest, nil, fmt.Errorf("authIndex is required")
		}
		if err := store.clearDegradation(strings.TrimSpace(payload.AuthIndex)); err != nil {
			return http.StatusInternalServerError, nil, err
		}
		return jsonAPIResult(map[string]any{"cleared": true}, nil)
	}
	if method == http.MethodGet && path == "/api/logs/groups" {
		category := strings.TrimSpace(query.Get("category"))
		groups, err := store.listLogGroups(category)
		return jsonAPIResult(map[string]any{"items": groups, "total": len(groups), "category": category}, err)
	}
	if method == http.MethodGet && path == "/api/logs" {
		limit, _ := strconv.Atoi(query.Get("limit"))
		settings, err := store.settings()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		category := strings.TrimSpace(query.Get("category"))
		search := strings.TrimSpace(query.Get("search"))
		if search == "" {
			search = strings.TrimSpace(query.Get("q"))
		}
		logs, err := store.listLogs(limit, settings.DebugEnabled, category, search, strings.TrimSpace(query.Get("groupId")), strings.TrimSpace(query.Get("status")))
		return jsonAPIResult(logs, err)
	}
	return http.StatusNotFound, nil, fmt.Errorf("API path not found")
}

func (controller *runtimeController) startAuthRefreshProxyBatch(store *guardianStore, body []byte) (int, []byte, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, fmt.Errorf("invalid request body")
	}
	proxyURLs, err := parseRefreshProxyURLLines(payload.Text)
	if err != nil {
		return http.StatusBadRequest, nil, err
	}

	var jobIDBytes [16]byte
	if _, err := rand.Read(jobIDBytes[:]); err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("create batch job ID: %w", err)
	}
	job := &authRefreshProxyBatchJob{
		ID:              hex.EncodeToString(jobIDBytes[:]),
		Status:          "queued",
		ProxyCount:      len(proxyURLs),
		FailedAuthNames: make([]string, 0, 10),
	}

	controller.mutex.Lock()
	if controller.store != store {
		controller.mutex.Unlock()
		return jsonAPIError(http.StatusServiceUnavailable, "plugin_stopping", "插件正在停止，未启动批量任务")
	}
	controller.refreshProxyBatchMutex.Lock()
	if active := controller.refreshProxyBatchJob; active != nil && (active.Status == "queued" || active.Status == "running") {
		activeJobID := active.ID
		controller.refreshProxyBatchMutex.Unlock()
		controller.mutex.Unlock()
		return jsonAPIResult(map[string]any{"status": "already_running", "jobId": activeJobID}, nil)
	}
	controller.refreshProxyBatchJob = job
	workerContext, cancel := context.WithCancel(context.Background())
	controller.refreshProxyBatchCancel = cancel
	controller.refreshProxyBatchGroup.Add(1)
	controller.refreshProxyBatchMutex.Unlock()
	controller.mutex.Unlock()

	go controller.runAuthRefreshProxyBatch(workerContext, store, job, proxyURLs)
	return jsonAPIResult(map[string]any{"jobId": job.ID, "status": "queued"}, nil)
}

func (controller *runtimeController) runAuthRefreshProxyBatch(ctx context.Context, store *guardianStore, job *authRefreshProxyBatchJob, proxyURLs []string) {
	defer controller.refreshProxyBatchGroup.Done()
	controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
		current.Status = "running"
	})

	store.authDistributionMutex.Lock()
	defer store.authDistributionMutex.Unlock()
	if ctx.Err() != nil {
		controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
			current.Status = "cancelled"
			current.Error = "任务已取消；尚未处理 auth 文件"
		})
		return
	}

	entries, err := listXAIAuthEntries()
	if err != nil {
		controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
			current.Status = "failed"
			current.Error = "读取 xAI auth 文件列表失败"
		})
		return
	}
	if len(entries) == 0 {
		controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
			current.Status = "failed"
			current.Error = "没有找到 xAI auth JSON 文件"
		})
		return
	}
	unusedProxyCount := len(proxyURLs) - len(entries)
	if unusedProxyCount < 0 {
		unusedProxyCount = 0
	}
	controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
		current.AuthCount = len(entries)
		current.UnusedProxyCount = unusedProxyCount
	})

	for index, entry := range entries {
		if ctx.Err() != nil {
			controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
				current.Status = "cancelled"
				current.Error = "任务已取消；可能已有部分 auth 文件写入"
			})
			return
		}
		saveErr := saveAndVerifyAuthRefreshProxyURL(entry, proxyURLs[index%len(proxyURLs)])
		controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
			current.ProcessedCount++
			if saveErr != nil {
				current.FailedCount++
				if len(current.FailedAuthNames) < 10 {
					current.FailedAuthNames = append(current.FailedAuthNames, authEntryName(entry))
				}
				return
			}
			current.UpdatedCount++
		})
	}
	controller.updateAuthRefreshProxyBatchJob(job, func(current *authRefreshProxyBatchJob) {
		current.Status = "completed"
	})
}

func (controller *runtimeController) updateAuthRefreshProxyBatchJob(job *authRefreshProxyBatchJob, update func(*authRefreshProxyBatchJob)) {
	controller.refreshProxyBatchMutex.Lock()
	update(job)
	controller.refreshProxyBatchMutex.Unlock()
}

func (controller *runtimeController) authRefreshProxyBatchStatusAPI(jobID string) (int, []byte, error) {
	controller.refreshProxyBatchMutex.RLock()
	job := controller.refreshProxyBatchJob
	if job == nil {
		controller.refreshProxyBatchMutex.RUnlock()
		return jsonAPIResult(map[string]any{"status": "idle"}, nil)
	}
	if jobID != "" && jobID != job.ID {
		controller.refreshProxyBatchMutex.RUnlock()
		return jsonAPIResult(map[string]any{"jobId": jobID, "status": "not_found"}, nil)
	}
	result := map[string]any{
		"jobId": job.ID, "status": job.Status, "authCount": job.AuthCount, "proxyCount": job.ProxyCount,
		"processedCount": job.ProcessedCount, "updatedCount": job.UpdatedCount, "failedCount": job.FailedCount,
		"failedAuthNames": append([]string(nil), job.FailedAuthNames...), "unusedProxyCount": job.UnusedProxyCount,
		"error": job.Error,
	}
	controller.refreshProxyBatchMutex.RUnlock()
	return jsonAPIResult(result, nil)
}

func (controller *runtimeController) updateSettings(store *guardianStore, body []byte) (int, []byte, error) {
	var payload struct {
		WorkerCount                                  int     `json:"workerCount"`
		ScheduleGroupCount                           int     `json:"scheduleGroupCount"`
		DebugEnabled                                 bool    `json:"debugEnabled"`
		RefreshIntervalSeconds                       int     `json:"refreshIntervalSeconds"`
		InspectionIntervalSeconds                    int     `json:"inspectionIntervalSeconds"`
		KeepaliveWorkerCount                         int     `json:"keepaliveWorkerCount"`
		KeepaliveIntervalSeconds                     int     `json:"keepaliveIntervalSeconds"`
		ReviveIntervalSeconds                        int     `json:"reviveIntervalSeconds"`
		ProbeRetryCount                              int     `json:"probeRetryCount"`
		MaxReviveFailureCount                        int     `json:"maxReviveFailureCount"`
		HealthySlotCount                             int     `json:"healthySlotCount"`
		HealthyCandidateSlotCount                    int     `json:"healthyCandidateSlotCount"`
		HealthySlotMaxAgeMinutes                     int     `json:"healthySlotMaxAgeMinutes"`
		RealtimeGuardTTFBSeconds                     float64 `json:"realtimeGuardTTFBSeconds"`
		RealtimeGuardGenerationSeconds               float64 `json:"realtimeGuardGenerationSeconds"`
		RealtimeGuardTokenThreshold                  int     `json:"realtimeGuardTokenThreshold"`
		QualityHardTPS                               float64 `json:"qualityHardTPS"`
		RealtimeGuardTimeoutSeconds                  int     `json:"realtimeGuardTimeoutSeconds"`
		RealtimeGuardIdleTimeoutSeconds              int     `json:"realtimeGuardIdleTimeoutSeconds"`
		RealtimeGuardMinSummaryChars                 int     `json:"realtimeGuardMinSummaryChars"`
		RealtimeGuardMinEncryptedBytes               int     `json:"realtimeGuardMinEncryptedBytes"`
		RealtimeGuardEncryptedBytesPerReasoningToken int     `json:"realtimeGuardEncryptedBytesPerReasoningToken"`
		RealtimeGuardMinOutputTokens                 int     `json:"realtimeGuardMinOutputTokens"`
		RealtimeGuardBurstMinReasoningTokens         int     `json:"realtimeGuardBurstMinReasoningTokens"`
		RealtimeGuardBurstMaxVisibleTokens           int     `json:"realtimeGuardBurstMaxVisibleTokens"`
		RealtimeGuardBurstMaxWindowMS                int     `json:"realtimeGuardBurstMaxWindowMs"`
		IPBatchRetentionDays                         int     `json:"ipBatchRetentionDays"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, err
	}
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store != store {
		return http.StatusServiceUnavailable, nil, fmt.Errorf("plugin is stopping")
	}
	currentSettings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	settings := pluginSettings{
		WorkerCount:                                  payload.WorkerCount,
		ScheduleGroupCount:                           payload.ScheduleGroupCount,
		DebugEnabled:                                 payload.DebugEnabled,
		RefreshIntervalSeconds:                       payload.RefreshIntervalSeconds,
		InspectionIntervalSeconds:                    currentSettings.InspectionIntervalSeconds,
		InspectionScheduleEnabled:                    currentSettings.InspectionScheduleEnabled,
		InspectionScheduleMode:                       currentSettings.InspectionScheduleMode,
		InspectionDailyTime:                          currentSettings.InspectionDailyTime,
		InspectionWorkerCount:                        currentSettings.InspectionWorkerCount,
		InspectionTimeoutSeconds:                     currentSettings.InspectionTimeoutSeconds,
		KeepaliveWorkerCount:                         payload.KeepaliveWorkerCount,
		KeepaliveIntervalSeconds:                     payload.KeepaliveIntervalSeconds,
		ReviveIntervalSeconds:                        payload.ReviveIntervalSeconds,
		ProbeRetryCount:                              payload.ProbeRetryCount,
		MaxReviveFailureCount:                        payload.MaxReviveFailureCount,
		HealthySlotCount:                             payload.HealthySlotCount,
		HealthyCandidateSlotCount:                    payload.HealthyCandidateSlotCount,
		HealthySlotMaxAgeMinutes:                     payload.HealthySlotMaxAgeMinutes,
		RealtimeGuardTTFBSeconds:                     payload.RealtimeGuardTTFBSeconds,
		RealtimeGuardGenerationSeconds:               payload.RealtimeGuardGenerationSeconds,
		RealtimeGuardTokenThreshold:                  payload.RealtimeGuardTokenThreshold,
		QualityHardTPS:                               payload.QualityHardTPS,
		RealtimeGuardTimeoutSeconds:                  payload.RealtimeGuardTimeoutSeconds,
		RealtimeGuardIdleTimeoutSeconds:              payload.RealtimeGuardIdleTimeoutSeconds,
		RealtimeGuardMinSummaryChars:                 payload.RealtimeGuardMinSummaryChars,
		RealtimeGuardMinEncryptedBytes:               payload.RealtimeGuardMinEncryptedBytes,
		RealtimeGuardEncryptedBytesPerReasoningToken: payload.RealtimeGuardEncryptedBytesPerReasoningToken,
		RealtimeGuardMinOutputTokens:                 payload.RealtimeGuardMinOutputTokens,
		RealtimeGuardBurstMinReasoningTokens:         payload.RealtimeGuardBurstMinReasoningTokens,
		RealtimeGuardBurstMaxVisibleTokens:           payload.RealtimeGuardBurstMaxVisibleTokens,
		RealtimeGuardBurstMaxWindowMS:                payload.RealtimeGuardBurstMaxWindowMS,
		IPBatchRetentionDays:                         payload.IPBatchRetentionDays,
	}
	slotSettingsChanged := currentSettings.HealthySlotCount != settings.HealthySlotCount || currentSettings.HealthyCandidateSlotCount != settings.HealthyCandidateSlotCount || currentSettings.HealthySlotMaxAgeMinutes != settings.HealthySlotMaxAgeMinutes
	if settings.ScheduleGroupCount != currentSettings.ScheduleGroupCount && controller.scheduleGroups.hasBusy() {
		return http.StatusConflict, nil, errScheduleGroupCountBusy
	}
	if err := store.setSettings(settings); err != nil {
		return http.StatusBadRequest, nil, err
	}
	if slotSettingsChanged {
		if err := store.reconcileHealthySlots(settings); err != nil {
			return http.StatusInternalServerError, nil, err
		}
		if err := refreshHealthyAuthDistribution(store); err != nil {
			_ = store.appendLog(logLevelError, "auth.distribution_failed", "健康槽位配置变更后刷新 auth 分配失败", sanitizeLogText(err.Error()))
			return http.StatusBadGateway, nil, err
		}
	}
	activeScheduleGroupCount := controller.runtimeScheduleGroupCount
	counterGroupCount := settings.ScheduleGroupCount
	if activeScheduleGroupCount > counterGroupCount {
		counterGroupCount = activeScheduleGroupCount
	}
	if err := store.reconcileScheduleGroupCounters(counterGroupCount); err != nil {
		return http.StatusInternalServerError, nil, err
	}
	persistedSettings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	_ = store.appendLog(logLevelInfo, "settings.updated", "插件配置已保存", "")
	return jsonAPIResult(publicSettings(persistedSettings), nil)
}

func (controller *runtimeController) updateInspectionSettings(store *guardianStore, body []byte) (int, []byte, error) {
	var payload struct {
		Enabled         *bool   `json:"enabled"`
		Mode            *string `json:"mode"`
		DailyTime       *string `json:"dailyTime"`
		IntervalSeconds *int    `json:"intervalSeconds"`
		Concurrency     *int    `json:"concurrency"`
		TimeoutSeconds  *int    `json:"timeoutSeconds"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, err
	}
	if payload.Enabled == nil || payload.Mode == nil || payload.DailyTime == nil || payload.IntervalSeconds == nil || payload.Concurrency == nil || payload.TimeoutSeconds == nil {
		return jsonAPIError(http.StatusBadRequest, "invalid_inspection_settings", "enabled、mode、dailyTime、intervalSeconds、concurrency 和 timeoutSeconds 均为必填项")
	}
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store != store {
		return jsonAPIError(http.StatusServiceUnavailable, "plugin_stopping", "插件正在停止，未保存巡检配置")
	}
	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	settings.InspectionScheduleEnabled = *payload.Enabled
	settings.InspectionScheduleMode = *payload.Mode
	settings.InspectionDailyTime = *payload.DailyTime
	settings.InspectionIntervalSeconds = *payload.IntervalSeconds
	settings.InspectionWorkerCount = *payload.Concurrency
	settings.InspectionTimeoutSeconds = *payload.TimeoutSeconds
	if err := store.setSettings(settings); err != nil {
		return http.StatusBadRequest, nil, err
	}
	controller.setInspectionScheduleLocked(inspectionScheduleFromSettings(settings))
	persistedSettings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	_ = store.appendLog(logLevelInfo, "inspection.settings_updated", "服务端巡检配置已保存", fmt.Sprintf("enabled=%t mode=%s interval_seconds=%d daily_time=%s concurrency=%d timeout_seconds=%d", persistedSettings.InspectionScheduleEnabled, persistedSettings.InspectionScheduleMode, persistedSettings.InspectionIntervalSeconds, persistedSettings.InspectionDailyTime, persistedSettings.InspectionWorkerCount, persistedSettings.InspectionTimeoutSeconds))
	response, err := controller.inspectionSettingsResponse(persistedSettings)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	return jsonAPIResult(response, nil)
}

func (controller *runtimeController) inspectionSettingsAPI(store *guardianStore) (int, []byte, error) {
	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	response, err := controller.inspectionSettingsResponse(settings)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	return jsonAPIResult(response, nil)
}

func (controller *runtimeController) inspectionSettingsResponse(settings pluginSettings) (map[string]any, error) {
	schedule, err := controller.currentInspectionScheduleStatus(settings)
	if err != nil {
		return nil, err
	}
	response := publicInspectionSettings(settings)
	response["nextRunAtMs"] = schedule.NextRunAtMS
	return response, nil
}

func (controller *runtimeController) keepaliveAPI(store *guardianStore) (int, []byte, error) {
	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	round, found, err := store.latestKeepaliveRound()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	controller.mutex.RLock()
	state := controller.keepaliveState
	controller.mutex.RUnlock()
	return jsonAPIResult(map[string]any{
		"settings": publicSettings(settings),
		"schedule": state.snapshot(settings.KeepaliveIntervalSeconds),
		"round":    mapKeepaliveRound(round, found),
	}, nil)
}

func (controller *runtimeController) runKeepaliveNow() (int, []byte, error) {
	controller.mutex.RLock()
	state := controller.keepaliveState
	controller.mutex.RUnlock()
	accepted, running := state.requestNow()
	if running {
		_, body, err := jsonAPIResult(map[string]any{"accepted": false, "running": true}, nil)
		return http.StatusConflict, body, err
	}
	return jsonAPIResult(map[string]any{"accepted": accepted, "running": false}, nil)
}

func (controller *runtimeController) queueInitialProbeAfterBatch() {
	controller.mutex.RLock()
	trigger := controller.probeTrigger
	controller.mutex.RUnlock()
	select {
	case trigger <- struct{}{}:
	default:
	}
}

func mapKeepaliveRound(round keepaliveRound, found bool) any {
	if !found {
		return nil
	}
	return round
}

func (controller *runtimeController) accountsAPI(store *guardianStore) (int, []byte, error) {
	inspection, err := store.latestAccountInspection()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	bindings, err := store.listAuthBindings(0)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	exitIPs := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		exitIPs[binding.AuthIndex] = binding.ExitIP
	}
	return jsonAPIResult(publicInspectionAccounts(inspection.Items, exitIPs), nil)
}

func (controller *runtimeController) refreshAccountsAPI(store *guardianStore) (int, []byte, error) {
	if err := refreshHealthyAuthDistribution(store); err != nil {
		_ = store.appendLog(logLevelError, "auth.distribution_failed", "手动刷新 auth 分配失败", sanitizeLogText(err.Error()))
		return http.StatusBadGateway, nil, err
	}
	return controller.accountsAPI(store)
}

func (controller *runtimeController) startAccountInspectionAPI(store *guardianStore) (int, []byte, error) {
	controller.mutex.Lock()
	if controller.store != store {
		controller.mutex.Unlock()
		return jsonAPIError(http.StatusServiceUnavailable, "plugin_stopping", "插件正在停止，未启动账号巡检")
	}
	if !inspectionMutex.TryLock() {
		latest, err := store.latestAccountInspection()
		controller.mutex.Unlock()
		if err != nil {
			return http.StatusInternalServerError, nil, err
		}
		if latest.Run == nil || latest.Run.Status != "running" {
			return jsonAPIError(http.StatusConflict, "inspection_starting", "已有账号巡检正在启动，请稍后刷新")
		}
		return jsonAPIResult(latest, nil)
	}

	runID, entries, previousResults, err := prepareAccountInspectionRun(store, "manual")
	if err != nil {
		inspectionMutex.Unlock()
		if runID > 0 {
			response, readErr := store.accountInspection(runID)
			controller.mutex.Unlock()
			if readErr != nil {
				return http.StatusInternalServerError, nil, readErr
			}
			return jsonAPIResult(response, nil)
		}
		controller.mutex.Unlock()
		return http.StatusBadGateway, nil, err
	}

	workerContext, cancel := context.WithCancel(context.Background())
	controller.manualInspectionCancel = cancel
	controller.inspectionGroup.Add(1)
	go func() {
		defer controller.inspectionGroup.Done()
		defer inspectionMutex.Unlock()
		if err := runPreparedAccountInspection(workerContext, store, runID, entries, previousResults); err != nil {
			_ = store.appendLog(logLevelError, "inspection.manual_failed", "手动 xAI 账号巡检失败", sanitizeLogText(err.Error()))
		}
	}()
	response, err := store.accountInspection(runID)
	controller.mutex.Unlock()
	return jsonAPIResult(response, err)
}

func (controller *runtimeController) inspectionAPI(store *guardianStore, query url.Values) (int, []byte, error) {
	runIDValue := strings.TrimSpace(query.Get("runId"))
	var runID int64
	if runIDValue != "" {
		parsedRunID, err := strconv.ParseInt(runIDValue, 10, 64)
		if err != nil || parsedRunID <= 0 {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid account inspection run ID")
		}
		runID = parsedRunID
	}
	settings, err := store.settings()
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	schedule, err := controller.currentInspectionScheduleStatus(settings)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	var response accountInspectionResponse
	if runID == 0 {
		response, err = store.latestAccountInspection()
	} else {
		response, err = store.accountInspection(runID)
		if errors.Is(err, sql.ErrNoRows) {
			return http.StatusNotFound, nil, fmt.Errorf("account inspection run not found")
		}
	}
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	return jsonAPIResult(map[string]any{"run": response.Run, "items": response.Items, "schedule": schedule}, nil)
}

func (controller *runtimeController) addIPBatch(store *guardianStore, body []byte) (int, []byte, error) {
	var payload struct {
		Text string `json:"text"`
		IPs  string `json:"ips"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest, nil, err
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		text = strings.TrimSpace(payload.IPs)
	}
	if text == "" {
		return http.StatusBadRequest, nil, fmt.Errorf("at least one IP is required")
	}
	nodes, inputErrors := parseProxyLines(text)
	if len(nodes) == 0 {
		return http.StatusBadRequest, nil, fmt.Errorf("no valid proxy nodes: %s", summarizeInputErrors(inputErrors))
	}
	batchID, added, duplicates, err := store.insertIPBatch(nodes, len(inputErrors))
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	_ = store.appendLog(logLevelInfo, "ip_batch.created", "降智守护 IP 批次已创建", fmt.Sprintf("批次 %s，新增 %d，重复 %d，格式错误 %d", batchID, added, duplicates, len(inputErrors)))
	if added > 0 {
		controller.queueInitialProbeAfterBatch()
	}
	return jsonAPIResult(map[string]any{"batchId": batchID, "added": added, "duplicates": duplicates, "errors": inputErrors}, nil)
}

func summarizeInputErrors(inputErrors []inputLineError) string {
	if len(inputErrors) == 0 {
		return "input is empty"
	}
	const maxDetails = 5
	details := make([]string, 0, min(len(inputErrors), maxDetails))
	for index, inputError := range inputErrors {
		if index == maxDetails {
			break
		}
		details = append(details, fmt.Sprintf("line %d: %s", inputError.Line, inputError.Message))
	}
	if len(inputErrors) > maxDetails {
		details = append(details, fmt.Sprintf("and %d more", len(inputErrors)-maxDetails))
	}
	return strings.Join(details, "; ")
}

func jsonAPIResult(value any, err error) (int, []byte, error) {
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "data": value})
	return http.StatusOK, raw, err
}

func jsonAPIError(status int, code, message string) (int, []byte, error) {
	raw, err := json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}})
	return status, raw, err
}

func publicSettings(settings pluginSettings) map[string]any {
	return map[string]any{
		"workerCount": settings.WorkerCount, "scheduleGroupCount": settings.ScheduleGroupCount, "debugEnabled": settings.DebugEnabled, "refreshIntervalSeconds": settings.RefreshIntervalSeconds,
		"inspectionIntervalSeconds": settings.InspectionIntervalSeconds, "inspectionScheduleEnabled": settings.InspectionScheduleEnabled,
		"inspectionScheduleMode": settings.InspectionScheduleMode, "inspectionDailyTime": settings.InspectionDailyTime,
		"inspectionWorkerCount": settings.InspectionWorkerCount, "inspectionTimeoutSeconds": settings.InspectionTimeoutSeconds,
		"keepaliveWorkerCount": settings.KeepaliveWorkerCount, "keepaliveIntervalSeconds": settings.KeepaliveIntervalSeconds,
		"reviveIntervalSeconds": settings.ReviveIntervalSeconds, "probeRetryCount": settings.ProbeRetryCount, "maxReviveFailureCount": settings.MaxReviveFailureCount,
		"healthySlotCount": settings.HealthySlotCount, "healthyCandidateSlotCount": settings.HealthyCandidateSlotCount, "healthySlotMaxAgeMinutes": settings.HealthySlotMaxAgeMinutes,
		"realtimeGuardTTFBSeconds": settings.RealtimeGuardTTFBSeconds, "realtimeGuardGenerationSeconds": settings.RealtimeGuardGenerationSeconds,
		"realtimeGuardTokenThreshold": settings.RealtimeGuardTokenThreshold, "qualityHardTPS": settings.QualityHardTPS, "realtimeGuardTimeoutSeconds": settings.RealtimeGuardTimeoutSeconds,
		"realtimeGuardIdleTimeoutSeconds": settings.RealtimeGuardIdleTimeoutSeconds, "realtimeGuardMinSummaryChars": settings.RealtimeGuardMinSummaryChars,
		"realtimeGuardMinEncryptedBytes": settings.RealtimeGuardMinEncryptedBytes, "realtimeGuardEncryptedBytesPerReasoningToken": settings.RealtimeGuardEncryptedBytesPerReasoningToken,
		"realtimeGuardMinOutputTokens": settings.RealtimeGuardMinOutputTokens, "realtimeGuardBurstMinReasoningTokens": settings.RealtimeGuardBurstMinReasoningTokens,
		"realtimeGuardBurstMaxVisibleTokens": settings.RealtimeGuardBurstMaxVisibleTokens, "realtimeGuardBurstMaxWindowMs": settings.RealtimeGuardBurstMaxWindowMS,
		"ipBatchRetentionDays": settings.IPBatchRetentionDays,
	}
}

func publicInspectionSettings(settings pluginSettings) map[string]any {
	return map[string]any{
		"enabled":         settings.InspectionScheduleEnabled,
		"mode":            settings.InspectionScheduleMode,
		"dailyTime":       settings.InspectionDailyTime,
		"intervalSeconds": settings.InspectionIntervalSeconds,
		"concurrency":     settings.InspectionWorkerCount,
		"timeoutSeconds":  settings.InspectionTimeoutSeconds,
	}
}

func publicNodes(nodes []proxyNode) []map[string]any {
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, map[string]any{"id": node.ID, "address": node.Address, "protocol": node.Protocol, "host": node.Host, "port": node.Port, "status": node.Status, "latencyMs": node.LatencyMS, "exitIp": node.ExitIP, "country": node.Country, "lastChecked": node.LastChecked, "lastError": sanitizeLogText(node.LastError), "createdAt": node.CreatedAt, "slotId": node.SlotID, "slotKind": node.SlotKind})
	}
	return items
}

func publicIPBatches(batches []ipBatch, retentionDays int) []map[string]any {
	items := make([]map[string]any, 0, len(batches))
	for _, batch := range batches {
		items = append(items, map[string]any{
			"batchId":             batch.ID,
			"sequenceNumber":      batch.SequenceNumber,
			"createdAt":           batch.CreatedAt,
			"expiresAt":           time.UnixMilli(batch.CreatedAt).AddDate(0, 0, retentionDays).UnixMilli(),
			"totalCount":          batch.TotalCount,
			"duplicateCount":      batch.DuplicateCount,
			"inputErrorCount":     batch.InputErrorCount,
			"completedCount":      batch.CompletedCount,
			"pendingCount":        batch.TotalCount - batch.CompletedCount,
			"currentHealthyCount": batch.CurrentHealthyCount,
		})
	}
	return items
}

func publicInspectionAccounts(results []accountInspectionResult, exitIPs map[string]string) []map[string]any {
	items := make([]map[string]any, 0, len(results))
	for _, result := range results {
		if !result.Probed {
			continue
		}
		items = append(items, map[string]any{
			"authIndex":      result.AuthIndex,
			"authName":       firstNonEmpty(result.DisplayAccount, result.FileName),
			"exitIp":         exitIPs[result.AuthIndex],
			"status":         result.Status,
			"priority":       result.Priority,
			"accountType":    firstNonEmpty(result.AccountType, result.PlanType),
			"scheduleGroup":  result.ScheduleGroup,
			"lastInspection": result.CreatedAtMS,
		})
	}
	return items
}

func publicAuthBindings(bindings []authBinding) []map[string]any {
	items := make([]map[string]any, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, map[string]any{
			"authIndex": binding.AuthIndex,
			"authName":  binding.AuthName,
			"slotId":    binding.SlotID,
			"proxyUrl":  binding.ProxyURL,
			"status":    binding.Status,
		})
	}
	return items
}

func publicDegradation(state degradationState) map[string]any {
	return map[string]any{"authIndex": state.AuthIndex, "authName": state.AuthName, "count": state.Count, "lastReason": state.LastReason, "lastRequestId": state.LastRequest, "lastSeen": state.LastSeen, "coolingUntil": state.CoolingUntil}
}
