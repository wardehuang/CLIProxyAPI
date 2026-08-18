package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

var pluginRuntime = &runtimeController{}

type runtimeController struct {
	mutex                sync.RWMutex
	topologyMutex        sync.Mutex
	realtimeGuardMutex   sync.Mutex
	store                *ipStore
	managerBaseURL       string
	managerManagementKey string
	workerCancel         context.CancelFunc
	workerGroup          sync.WaitGroup
	keepalive            *keepaliveScheduleState
}

func (controller *runtimeController) configure(config pluginConfig) error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	return controller.configureLocked(config)
}

func (controller *runtimeController) configureLocked(config pluginConfig) error {
	resolvedDatabasePath, err := resolveDatabasePath(config.DatabasePath)
	if err != nil {
		return err
	}
	if controller.store != nil {
		if controller.store.path != resolvedDatabasePath {
			_ = controller.store.appendLog(logLevelWarn, "plugin.reconfigure_deferred", 0, "", "插件配置变更将在下次启动时生效", fmt.Sprintf("数据库路径当前 %s；下次启动 %s", controller.store.path, resolvedDatabasePath))
			return nil
		}
		settings, settingsErr := controller.store.settings()
		if settingsErr != nil {
			return settingsErr
		}
		if configuredManagerBaseURL := strings.TrimSpace(config.ManagerBaseURL); configuredManagerBaseURL != "" {
			settings.ManagerBaseURL = strings.TrimRight(configuredManagerBaseURL, "/")
		}
		if configuredManagerManagementKey := strings.TrimSpace(config.ManagerManagementKey); configuredManagerManagementKey != "" {
			settings.ManagerManagementKey = configuredManagerManagementKey
		}
		if config.WorkerCount != 0 {
			settings.WorkerCount = config.WorkerCount
		}
		if err := controller.store.setSettings(settings); err != nil {
			return err
		}
		controller.managerBaseURL = settings.ManagerBaseURL
		controller.managerManagementKey = settings.ManagerManagementKey
		if config.WorkerCount != 0 {
			_ = controller.store.appendLog(logLevelInfo, "plugin.reconfigure_deferred", 0, "", "插件配置已保存，将在下次启动时生效", fmt.Sprintf("探测线程数 %d", config.WorkerCount))
		}
		return nil
	}

	store, err := openIPStore(config.DatabasePath)
	if err != nil {
		return err
	}
	settings, err := store.settings()
	if err != nil {
		_ = store.close()
		return err
	}
	if config.WorkerCount != 0 {
		settings.WorkerCount = config.WorkerCount
	}
	if configuredManagerBaseURL := strings.TrimSpace(config.ManagerBaseURL); configuredManagerBaseURL != "" {
		settings.ManagerBaseURL = strings.TrimRight(configuredManagerBaseURL, "/")
	}
	if configuredManagerManagementKey := strings.TrimSpace(config.ManagerManagementKey); configuredManagerManagementKey != "" {
		settings.ManagerManagementKey = configuredManagerManagementKey
	}
	if config.WorkerCount != 0 || strings.TrimSpace(config.ManagerBaseURL) != "" || strings.TrimSpace(config.ManagerManagementKey) != "" {
		if err := store.setSettings(settings); err != nil {
			_ = store.close()
			return err
		}
	}
	if err := store.reconcileSlotLayout(pluginSettings{}, settings); err != nil {
		_ = store.close()
		return err
	}

	controller.store = store
	controller.managerBaseURL = settings.ManagerBaseURL
	controller.managerManagementKey = settings.ManagerManagementKey
	if controller.keepalive == nil {
		controller.keepalive = newKeepaliveScheduleState()
	}
	controller.keepalive.markWaiting(time.Now())
	controller.topologyMutex.Lock()
	defer controller.topologyMutex.Unlock()
	if err := refreshHealthyAuthDistribution(store, settings.HealthySlotCount); err != nil {
		_ = store.appendLog(logLevelError, "auth_distribution.startup_failed", 0, "", "插件启动时同步 xAI auth proxy_url 失败", err.Error())
	}
	_ = store.appendLog(
		logLevelInfo,
		"plugin.configured",
		0,
		"",
		"xAi出口守护插件已启动",
		fmt.Sprintf("数据库 %s，探测线程数 %d，保活线程数 %d，保活间隔 %d 秒，复活间隔 %d 秒", store.path, settings.WorkerCount, settings.KeepaliveWorkerCount, settings.KeepaliveIntervalSeconds, settings.ReviveIntervalSeconds),
	)
	controller.startWorkersLocked(store, settings)
	return nil
}

func pluginSettingsEqual(left, right pluginSettings) bool {
	return left.WorkerCount == right.WorkerCount &&
		left.RefreshIntervalSeconds == right.RefreshIntervalSeconds &&
		left.KeepaliveWorkerCount == right.KeepaliveWorkerCount &&
		left.KeepaliveIntervalSeconds == right.KeepaliveIntervalSeconds &&
		left.ReviveIntervalSeconds == right.ReviveIntervalSeconds &&
		left.ProbeRetryCount == right.ProbeRetryCount &&
		left.HealthySlotCount == right.HealthySlotCount &&
		left.HealthyCandidateSlotCount == right.HealthyCandidateSlotCount &&
		left.HealthySlotMaxAgeMinutes == right.HealthySlotMaxAgeMinutes &&
		left.QualityWorkerCount == right.QualityWorkerCount &&
		left.QualityProbeTimeoutSeconds == right.QualityProbeTimeoutSeconds &&
		left.QualitySoftTPS == right.QualitySoftTPS &&
		left.QualityHardTPS == right.QualityHardTPS &&
		left.DebugEnabled == right.DebugEnabled &&
		left.Grok2apiSyncEnabled == right.Grok2apiSyncEnabled &&
		left.Grok2apiBaseUrl == right.Grok2apiBaseUrl &&
		left.Grok2apiAdminUsername == right.Grok2apiAdminUsername &&
		left.Grok2apiAdminPassword == right.Grok2apiAdminPassword &&
		left.ManagerBaseURL == right.ManagerBaseURL &&
		left.ManagerManagementKey == right.ManagerManagementKey
}

func (controller *runtimeController) ensure() error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store != nil {
		return nil
	}
	return controller.configureLocked(pluginConfig{DatabasePath: defaultDatabasePath})
}

func (controller *runtimeController) withStore(fn func(*ipStore) ([]byte, error)) ([]byte, error) {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	if controller.store == nil {
		return nil, fmt.Errorf("plugin store is not initialized")
	}
	return fn(controller.store)
}

func (controller *runtimeController) currentSettings() (pluginSettings, error) {
	controller.mutex.RLock()
	defer controller.mutex.RUnlock()
	if controller.store == nil {
		return pluginSettings{}, fmt.Errorf("plugin store is not initialized")
	}
	return controller.store.settings()
}

func (controller *runtimeController) updateSettings(store *ipStore, settings pluginSettings) (bool, error) {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.store != store {
		return false, fmt.Errorf("plugin store changed during settings update")
	}
	currentSettings, err := store.settings()
	if err != nil {
		return false, err
	}
	if pluginSettingsEqual(currentSettings, settings) {
		return false, nil
	}
	if err := store.setSettings(settings); err != nil {
		return false, err
	}
	controller.managerBaseURL = settings.ManagerBaseURL
	controller.managerManagementKey = settings.ManagerManagementKey
	return true, nil
}

func (controller *runtimeController) startWorkersLocked(store *ipStore, settings pluginSettings) {
	workerContext, cancel := context.WithCancel(context.Background())
	controller.workerCancel = cancel
	for workerIndex := 0; workerIndex < settings.WorkerCount; workerIndex++ {
		controller.workerGroup.Add(1)
		go func() {
			defer controller.workerGroup.Done()
			runProbeWorker(workerContext, store, settings.ProbeRetryCount)
		}()
	}
	controller.workerGroup.Add(1)
	go func() {
		defer controller.workerGroup.Done()
		runKeepaliveScheduler(workerContext, store, settings)
	}()
	controller.workerGroup.Add(1)
	go func() {
		defer controller.workerGroup.Done()
		runReviveScheduler(workerContext, store, settings)
	}()
}

func (controller *runtimeController) stopWorkersLocked() {
	if controller.workerCancel == nil {
		return
	}
	controller.workerCancel()
	controller.workerGroup.Wait()
	controller.workerCancel = nil
}

func (controller *runtimeController) shutdown() {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	controller.stopWorkersLocked()
	if controller.store != nil {
		_ = controller.store.appendLog(logLevelInfo, "plugin.shutdown", 0, "", "xAi出口守护插件正在停止", "探测线程已停止")
		_ = controller.store.close()
		controller.store = nil
	}
}
