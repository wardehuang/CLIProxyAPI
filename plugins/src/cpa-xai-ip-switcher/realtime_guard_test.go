package main

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyRealtimeGuardUsesHTTPFirstResponseByteForTTFB(t *testing.T) {
	startedAt := time.Now().Add(-20 * time.Second)
	firstResponseByteAt := startedAt.Add(time.Second)
	firstPayloadAt := startedAt.Add(10 * time.Second)
	finishedAt := firstPayloadAt.Add(500 * time.Millisecond)
	probe := realtimeGuardProbe{
		StatusCode:          http.StatusOK,
		Completed:           true,
		StartedAt:           startedAt.Add(-10 * time.Second),
		UpstreamStartedAt:   startedAt,
		FirstResponseByteAt: firstResponseByteAt,
		FirstPayloadAt:      firstPayloadAt,
		FinishedAt:          finishedAt,
		Body:                []byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":101}}}\n\n"),
	}
	settings := pluginSettings{
		QualitySoftTPS:                     10_000,
		QualityHardTPS:                     20_000,
		RealtimeGuardTTFBSeconds:           5,
		RealtimeGuardGenerationSeconds:     1,
		RealtimeGuardTokenThreshold:        100,
		RealtimeGuardTimeoutSeconds:        30,
		RealtimeGuardMinSummaryChars:       1_000,
		RealtimeGuardMinEncryptedBytes:     1_000,
		RealtimeGuardMinOutputTokens:       1_000,
		RealtimeGuardBurstMaxVisibleTokens: 1,
	}

	decision := classifyRealtimeGuardProbeWithSettings(probe, settings)

	if decision.Action != realtimeGuardActionFlush || decision.Reason != "within_threshold" {
		t.Fatalf("decision = %+v, want flush within_threshold", decision)
	}
	if decision.TTFBMs != 1_000 {
		t.Fatalf("TTFBMs = %d, want 1000 from HTTP first response byte", decision.TTFBMs)
	}
	if decision.GenerationMs != 500 {
		t.Fatalf("GenerationMs = %d, want 500 from first payload to finish", decision.GenerationMs)
	}
}

func TestClassifyRealtimeGuardRejectsInvalidTimingOrder(t *testing.T) {
	startedAt := time.Now().Add(-5 * time.Second)
	cases := []struct {
		name   string
		probe  realtimeGuardProbe
		reason string
	}{
		{
			name: "first response byte before upstream start",
			probe: realtimeGuardProbe{
				StatusCode:          http.StatusOK,
				Completed:           true,
				StartedAt:           startedAt,
				UpstreamStartedAt:   startedAt.Add(time.Second),
				FirstResponseByteAt: startedAt.Add(500 * time.Millisecond),
				FirstPayloadAt:      startedAt.Add(2 * time.Second),
				FinishedAt:          startedAt.Add(3 * time.Second),
				Body:                []byte("data: {\"type\":\"response.completed\"}\n\n"),
			},
			reason: "upstream_ttfb_invalid",
		},
		{
			name: "first payload before first response byte",
			probe: realtimeGuardProbe{
				StatusCode:          http.StatusOK,
				Completed:           true,
				StartedAt:           startedAt,
				UpstreamStartedAt:   startedAt,
				FirstResponseByteAt: startedAt.Add(2 * time.Second),
				FirstPayloadAt:      startedAt.Add(time.Second),
				FinishedAt:          startedAt.Add(3 * time.Second),
				Body:                []byte("data: {\"type\":\"response.completed\"}\n\n"),
			},
			reason: "first_payload_invalid",
		},
		{
			name: "finished before first payload",
			probe: realtimeGuardProbe{
				StatusCode:          http.StatusOK,
				Completed:           true,
				StartedAt:           startedAt,
				UpstreamStartedAt:   startedAt,
				FirstResponseByteAt: startedAt.Add(time.Second),
				FirstPayloadAt:      startedAt.Add(3 * time.Second),
				FinishedAt:          startedAt.Add(2 * time.Second),
				Body:                []byte("data: {\"type\":\"response.completed\"}\n\n"),
			},
			reason: "generation_window_invalid",
		},
		{
			name: "finished timestamp missing",
			probe: realtimeGuardProbe{
				StatusCode:          http.StatusOK,
				Completed:           true,
				StartedAt:           startedAt,
				UpstreamStartedAt:   startedAt,
				FirstResponseByteAt: startedAt.Add(time.Second),
				FirstPayloadAt:      startedAt.Add(2 * time.Second),
				Body:                []byte("data: {\"type\":\"response.completed\"}\n\n"),
			},
			reason: "finished_at_missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := classifyRealtimeGuardProbeWithSettings(tc.probe, pluginSettings{RealtimeGuardTimeoutSeconds: 30})
			if decision.Action != realtimeGuardActionFail || decision.Reason != tc.reason {
				t.Fatalf("decision = %+v, want fail %s", decision, tc.reason)
			}
		})
	}
}

func TestClassifyRealtimeGuardRejectsMissingHTTPFirstResponseByte(t *testing.T) {
	startedAt := time.Now().Add(-2 * time.Second)
	probe := realtimeGuardProbe{
		StatusCode:        http.StatusOK,
		Completed:         true,
		StartedAt:         startedAt,
		UpstreamStartedAt: startedAt,
		FirstPayloadAt:    startedAt.Add(time.Second),
		FinishedAt:        startedAt.Add(1500 * time.Millisecond),
		Body:              []byte("data: {\"type\":\"response.completed\"}\n\n"),
	}

	decision := classifyRealtimeGuardProbeWithSettings(probe, pluginSettings{RealtimeGuardTimeoutSeconds: 30})

	if decision.Action != realtimeGuardActionFail || decision.Reason != "upstream_first_byte_missing" {
		t.Fatalf("decision = %+v, want fail upstream_first_byte_missing", decision)
	}
}
