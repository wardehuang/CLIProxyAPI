package main

import (
	"net/http"
	"time"
)

type realtimeGuardClassification string

type realtimeGuardQualityLevel string

const (
	realtimeGuardClassificationUnknown     realtimeGuardClassification = "unknown"
	realtimeGuardClassificationDegradation realtimeGuardClassification = "suspected_degradation"
	realtimeGuardClassificationTransient   realtimeGuardClassification = "transient_upstream_error"
	realtimeGuardClassificationNormal      realtimeGuardClassification = "normal"
	realtimeGuardQualityUnknown            realtimeGuardQualityLevel   = "unknown"
	realtimeGuardQualityHard               realtimeGuardQualityLevel   = "hard"
	realtimeGuardQualitySoft               realtimeGuardQualityLevel   = "soft"
	realtimeGuardQualityHealthy            realtimeGuardQualityLevel   = "healthy"
	realtimeGuardActionFlush                                           = "flush"
	realtimeGuardActionRetry                                           = "retry"
	realtimeGuardActionFail                                            = "fail"
)

type realtimeGuardProbe struct {
	RequestID       string
	Provider        string
	SourceFormat    string
	Model           string
	RequestedModel  string
	AuthID          string
	AuthIndex       string
	AuthFileName    string
	ProxyURL        string
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	Body            []byte
	StatusCode      int
	Error           string
	Completed       bool
	StartedAt       time.Time
	FirstPayloadAt  time.Time
	FinishedAt      time.Time
	RetryCount      int
	MaxRetries      int
	Metadata        map[string]any
	SourceSnapshot  realtimeGuardSourceSnapshot
}

type realtimeGuardDecision struct {
	Action              string
	Reason              string
	Classification      realtimeGuardClassification
	QualityLevel        realtimeGuardQualityLevel
	TPS                 float64
	TotalDurationMs     int64
	TTFBMs              int64
	GenerationMs        int64
	TotalTokens         int64
	Error               string
	OriginalNodeID      int64
	ReplacementNodeID   int64
	OriginalProxyURL    string
	ReplacementProxyURL string
}
