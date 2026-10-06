package main

func defaultPluginSettings() pluginSettings {
	return pluginSettings{
		WorkerCount:                                  defaultWorkerCount,
		ScheduleGroupCount:                           defaultScheduleGroupCount,
		DebugEnabled:                                 defaultDebugEnabled,
		RefreshIntervalSeconds:                       defaultRefreshIntervalSeconds,
		InspectionIntervalSeconds:                    defaultInspectionIntervalSeconds,
		KeepaliveWorkerCount:                         defaultKeepaliveWorkerCount,
		KeepaliveIntervalSeconds:                     defaultKeepaliveIntervalSeconds,
		ReviveIntervalSeconds:                        defaultReviveIntervalSeconds,
		ProbeRetryCount:                              defaultProbeRetryCount,
		HealthySlotCount:                             defaultHealthySlotCount,
		HealthyCandidateSlotCount:                    defaultHealthyCandidateCount,
		HealthySlotMaxAgeMinutes:                     defaultHealthySlotMaxAgeMinutes,
		RealtimeGuardTTFBSeconds:                     defaultRealtimeGuardTTFBSeconds,
		RealtimeGuardGenerationSeconds:               defaultRealtimeGuardGenerationSeconds,
		RealtimeGuardTokenThreshold:                  defaultRealtimeGuardTokenThreshold,
		RealtimeGuardTimeoutSeconds:                  defaultRealtimeGuardTimeoutSeconds,
		RealtimeGuardIdleTimeoutSeconds:              defaultRealtimeGuardIdleTimeoutSeconds,
		RealtimeGuardMinSummaryChars:                 defaultRealtimeGuardMinSummaryChars,
		RealtimeGuardMinEncryptedBytes:               defaultRealtimeGuardMinEncryptedBytes,
		RealtimeGuardEncryptedBytesPerReasoningToken: defaultRealtimeGuardEncryptedBytesPerReasoningToken,
		RealtimeGuardMinOutputTokens:                 defaultRealtimeGuardMinOutputTokens,
		RealtimeGuardBurstMinReasoningTokens:         defaultRealtimeGuardBurstMinReasoningTokens,
		RealtimeGuardBurstMaxVisibleTokens:           defaultRealtimeGuardBurstMaxVisibleTokens,
		RealtimeGuardBurstMaxWindowMS:                defaultRealtimeGuardBurstMaxWindowMS,
		IPBatchRetentionDays:                         defaultIPBatchRetentionDays,
	}
}
