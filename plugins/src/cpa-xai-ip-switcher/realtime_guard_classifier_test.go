package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func marshalRealtimeGuardSSE(t *testing.T, events ...map[string]any) []byte {
	t.Helper()
	var builder strings.Builder
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		builder.WriteString("data: ")
		builder.Write(payload)
		builder.WriteString("\n\n")
	}
	return []byte(builder.String())
}

func completedMessageItem(text string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     "msg-1",
		"status": "completed",
		"content": []any{
			map[string]any{"type": "output_text", "text": text},
		},
	}
}

func completedRefusalItem() map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     "msg-1",
		"status": "completed",
		"content": []any{
			map[string]any{"type": "refusal", "refusal": "No."},
		},
	}
}

func completedFunctionCallItem() map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   "tool-1",
		"name":      "patch",
		"arguments": `{"path":"example.go"}`,
		"status":    "completed",
	}
}

func completedReasoningItem(summary, encrypted string) map[string]any {
	item := map[string]any{
		"type":              "reasoning",
		"id":                "reason-1",
		"status":            "completed",
		"encrypted_content": encrypted,
	}
	if summary != "" {
		item["summary"] = []any{
			map[string]any{"type": "summary_text", "text": summary},
		}
	}
	return item
}

func completedResponseSSE(t *testing.T, outputTokens, reasoningTokens int, items ...map[string]any) []byte {
	t.Helper()
	output := make([]any, 0, len(items))
	events := make([]map[string]any, 0, len(items)+1)
	for _, item := range items {
		output = append(output, item)
		events = append(events, map[string]any{"type": "response.output_item.done", "item": item})
	}
	events = append(events, map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"usage": map[string]any{
				"output_tokens": outputTokens,
				"output_tokens_details": map[string]any{
					"reasoning_tokens": reasoningTokens,
				},
			},
			"output": output,
		},
	})
	return marshalRealtimeGuardSSE(t, events...)
}

func classifyRealtimeGuardCase(t *testing.T, body, request []byte, generation time.Duration, tweak func(*realtimeGuardProbe, *pluginSettings)) realtimeGuardDecision {
	t.Helper()
	settings := defaultPluginSettings()
	startedAt := time.Unix(1_700_000_000, 0).UTC()
	firstResponseByteAt := startedAt.Add(20 * time.Millisecond)
	firstPayloadAt := firstResponseByteAt.Add(20 * time.Millisecond)
	probe := realtimeGuardProbe{
		RequestID:           "req-test",
		TraceID:             "trace-test",
		AuthIndex:           "auth-1",
		StatusCode:          http.StatusOK,
		Completed:           true,
		StartedAt:           startedAt,
		UpstreamStartedAt:   startedAt,
		FirstResponseByteAt: firstResponseByteAt,
		FirstPayloadAt:      firstPayloadAt,
		FinishedAt:          firstPayloadAt.Add(generation),
		Body:                body,
		OriginalRequest:     request,
	}
	if tweak != nil {
		tweak(&probe, &settings)
	}
	return classifyRealtimeGuardProbeWithSettings(probe, settings)
}

func readonlyTerminalRequest(t *testing.T) []byte {
	t.Helper()
	user := map[string]any{"role": "user", "content": "Inspect the workspace."}
	items := []map[string]any{user}
	for index, command := range []string{"SELECT 1", "SELECT sqlite_version()", "go env GOMODCACHE GOPATH"} {
		callID := "terminal-" + string(rune('1'+index))
		call := mutationCallForTest("terminal", callID)
		arguments, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		call["arguments"] = string(arguments)
		items = append(items, call, mutationOutputForTest(callID, `{"exit_code":0,"error":null,"success":true,"verified":true}`))
	}
	return mutationRequestForTest(t, items...)
}

func TestClassifyRealtimeGuardMissingThinkingWithoutAction(t *testing.T) {
	body := completedResponseSSE(t, 107, 0, completedMessageItem("Task finished."))
	request := readonlyTerminalRequest(t)

	decision := classifyRealtimeGuardCase(t, body, request, 5674*time.Millisecond, nil)
	if decision.Action != realtimeGuardActionRetry ||
		decision.Classification != realtimeGuardClassificationDegradation ||
		decision.QualityLevel != realtimeGuardQualitySoft ||
		decision.Reason != "missing_thinking_without_action" ||
		decision.CompletedMutationEvidence ||
		decision.MutationEvidence.Matched ||
		decision.MutationEvidence.ScanStartIndex != 1 {
		t.Fatalf("production-like TPS decision = %+v", decision)
	}

	lowTPS := classifyRealtimeGuardCase(t, body, request, 5674*time.Millisecond, func(_ *realtimeGuardProbe, settings *pluginSettings) {
		settings.QualitySoftTPS = 10_000
		settings.QualityHardTPS = 20_000
	})
	if lowTPS.Reason != "missing_thinking_without_action" || lowTPS.Action != realtimeGuardActionRetry {
		t.Fatalf("TPS below soft still must retry, got %+v", lowTPS)
	}
}

func TestClassifyRealtimeGuardCompletedToolCallDoesNotScanInput(t *testing.T) {
	request := mutationRequestForTest(t,
		map[string]any{"role": "user", "content": "Update the implementation."},
		mutationCallForTest("write_file", "write-1"),
		mutationOutputForTest("write-1", `{"verified":true}`),
	)

	toolOnly := classifyRealtimeGuardCase(t,
		completedResponseSSE(t, 40, 0, completedFunctionCallItem()),
		request,
		2*time.Second,
		nil,
	)
	if toolOnly.Reason != "completed_tool_call_evidence" ||
		toolOnly.Action != realtimeGuardActionFlush ||
		toolOnly.MutationEvidence.ScanStartIndex != -1 ||
		!toolOnly.CompletedToolCallEvidence {
		t.Fatalf("tool-only decision = %+v", toolOnly)
	}

	withPreamble := classifyRealtimeGuardCase(t,
		completedResponseSSE(t, 40, 0, completedMessageItem("Calling patch."), completedFunctionCallItem()),
		request,
		2*time.Second,
		nil,
	)
	if withPreamble.Reason != "completed_tool_call_evidence" || withPreamble.MutationEvidence.ScanStartIndex != -1 {
		t.Fatalf("preamble+tool decision = %+v", withPreamble)
	}
}

func TestClassifyRealtimeGuardCompletedMutationEvidence(t *testing.T) {
	user := map[string]any{"role": "user", "content": "Update the implementation."}
	body := completedResponseSSE(t, 40, 0, completedMessageItem("Write completed."))

	writeOK := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, user, mutationCallForTest("write_file", "write-1"), mutationOutputForTest("write-1", `{"verified":true}`)), 2*time.Second, nil)
	if writeOK.Reason != "completed_mutation_evidence" ||
		writeOK.Action != realtimeGuardActionFlush ||
		writeOK.Classification != realtimeGuardClassificationNormal ||
		!writeOK.CompletedMutationEvidence ||
		writeOK.MutationEvidence != (completedMutationEvidence{Matched: true, ScanStartIndex: 1, ToolName: "write_file", CallID: "write-1"}) {
		t.Fatalf("write_file decision = %+v", writeOK)
	}

	patchOK := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, user, mutationCallForTest("patch", "edit-1"), mutationOutputForTest("edit-1", `{"success":true}`)), 2*time.Second, nil)
	if patchOK.Reason != "completed_mutation_evidence" || patchOK.MutationEvidence.ToolName != "patch" {
		t.Fatalf("patch decision = %+v", patchOK)
	}

	skillOK := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, user, mutationCallForTest("skill_manage", "skill-1"), mutationOutputForTest("skill-1", `{"success":true}`)), 2*time.Second, nil)
	if skillOK.Reason != "completed_mutation_evidence" || skillOK.MutationEvidence.ToolName != "skill_manage" {
		t.Fatalf("skill_manage decision = %+v", skillOK)
	}

	failed := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, user, mutationCallForTest("patch", "edit-1"), mutationOutputForTest("edit-1", `{"success":false}`)), 2*time.Second, nil)
	if failed.Reason != "missing_thinking_without_action" || failed.CompletedMutationEvidence {
		t.Fatalf("failed patch must retry, got %+v", failed)
	}
}

func TestClassifyRealtimeGuardMutationWindowAndRefusal(t *testing.T) {
	body := completedResponseSSE(t, 40, 0, completedMessageItem("Write completed."))
	previousUser := map[string]any{"role": "user", "content": "Previous task."}
	currentUser := map[string]any{"role": "user", "content": "New task."}
	assistant := map[string]any{"role": "assistant", "content": "Previous task is complete."}
	success := mutationOutputForTest("edit-1", `{"success":true}`)
	patch := mutationCallForTest("patch", "edit-1")

	stale := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, previousUser, patch, success, assistant, currentUser), 2*time.Second, nil)
	if stale.Reason != "missing_thinking_without_action" || stale.MutationEvidence.ScanStartIndex != 5 {
		t.Fatalf("independent previous task must not count, got %+v", stale)
	}

	continuation := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, previousUser, patch, success, currentUser), 2*time.Second, nil)
	if continuation.Reason != "completed_mutation_evidence" || continuation.MutationEvidence.ScanStartIndex != 1 {
		t.Fatalf("adjacent continuation must keep fallback scan, got %+v", continuation)
	}

	terminalWrite := mutationCallForTest("terminal", "terminal-1")
	terminalWrite["arguments"] = `{"command":"printf changed > example.txt"}`
	shell := classifyRealtimeGuardCase(t, body, mutationRequestForTest(t, currentUser, terminalWrite, mutationOutputForTest("terminal-1", `{"exit_code":0,"error":null}`)), 2*time.Second, nil)
	if shell.Reason != "missing_thinking_without_action" || shell.CompletedMutationEvidence {
		t.Fatalf("successful terminal write must not count, got %+v", shell)
	}

	refusal := classifyRealtimeGuardCase(t,
		completedResponseSSE(t, 40, 0, completedRefusalItem()),
		mutationRequestForTest(t, currentUser, patch, success),
		2*time.Second,
		nil,
	)
	if refusal.Reason != "missing_thinking_without_action" ||
		refusal.MutationEvidence.ScanStartIndex != -1 ||
		refusal.CompletedMutationEvidence {
		t.Fatalf("refusal must not use mutation exemption, got %+v", refusal)
	}
}

func TestClassifyRealtimeGuardThinkingPolicyAndHardTPS(t *testing.T) {
	short := classifyRealtimeGuardCase(t, completedResponseSSE(t, 5, 0, completedMessageItem("OK")), nil, 2*time.Second, nil)
	if short.Action != realtimeGuardActionFlush || short.Reason != "within_threshold" || !short.IsRealThinking || short.RealThinkingReason != "below_minimum_output_tokens" {
		t.Fatalf("short output decision = %+v", short)
	}

	summary := strings.Repeat("s", 32)
	realSummary := classifyRealtimeGuardCase(t, completedResponseSSE(t, 40, 0, completedReasoningItem(summary, ""), completedMessageItem("Done.")), nil, 2*time.Second, nil)
	if realSummary.Action != realtimeGuardActionFlush || realSummary.Reason != "within_threshold" || realSummary.RealThinkingReason != "summary_evidence" || realSummary.MutationEvidence.ScanStartIndex != -1 {
		t.Fatalf("summary evidence decision = %+v", realSummary)
	}

	encrypted := classifyRealtimeGuardCase(t, completedResponseSSE(t, 40, 0, completedReasoningItem("", strings.Repeat("e", 256)), completedMessageItem("Done.")), nil, 2*time.Second, nil)
	if encrypted.RealThinkingReason != "encrypted_evidence" || encrypted.MutationEvidence.ScanStartIndex != -1 {
		t.Fatalf("encrypted evidence decision = %+v", encrypted)
	}

	tokensOnly := classifyRealtimeGuardCase(t, completedResponseSSE(t, 100, 50, completedMessageItem("Done.")), nil, 2*time.Second, nil)
	if tokensOnly.Reason != "missing_thinking_without_action" || tokensOnly.RealThinkingReason != "reasoning_tokens_without_evidence" {
		t.Fatalf("reasoning tokens must not substitute evidence, got %+v", tokensOnly)
	}

	burst := classifyRealtimeGuardCase(t, completedResponseSSE(t, 90, 80, completedMessageItem("OK")), nil, 200*time.Millisecond, func(probe *realtimeGuardProbe, _ *pluginSettings) {
		probe.FirstVisibleAt = probe.FirstPayloadAt
	})
	if burst.Reason != "within_threshold" || burst.RealThinkingReason != "burst_dump_disabled" || !burst.IsRealThinking {
		t.Fatalf("burst exemption decision = %+v", burst)
	}

	hard := classifyRealtimeGuardCase(t, completedResponseSSE(t, 2000, 0, completedFunctionCallItem()), nil, time.Second, nil)
	if hard.Reason != "hard_tps" || hard.QualityLevel != realtimeGuardQualityHard || hard.Action != realtimeGuardActionRetry {
		t.Fatalf("hard TPS must override tool-call evidence, got %+v", hard)
	}

	ttfb := classifyRealtimeGuardCase(t, completedResponseSSE(t, 400, 0, completedReasoningItem(summary, ""), completedMessageItem("Done.")), nil, 500*time.Millisecond, func(probe *realtimeGuardProbe, settings *pluginSettings) {
		probe.FirstResponseByteAt = probe.UpstreamStartedAt.Add(6 * time.Second)
		probe.FirstPayloadAt = probe.FirstResponseByteAt.Add(20 * time.Millisecond)
		probe.FinishedAt = probe.FirstPayloadAt.Add(500 * time.Millisecond)
		settings.RealtimeGuardTTFBSeconds = 5
		settings.RealtimeGuardGenerationSeconds = 1.25
		settings.RealtimeGuardTokenThreshold = 300
	})
	if ttfb.Reason == "ttfb_downgrade" {
		t.Fatalf("TTFB rejection must stay disabled, got %+v", ttfb)
	}
}

func TestRealtimeGuardRetryMode(t *testing.T) {
	if got := realtimeGuardRetryMode(realtimeGuardDecision{Reason: executor.ErrStreamProgressTimeout.Error()}); got != pluginapi.StreamCompletionRetryModeReloadAndExcludeSelectedAuth {
		t.Fatalf("progress timeout retry mode = %q", got)
	}
	if got := realtimeGuardRetryMode(realtimeGuardDecision{Classification: realtimeGuardClassificationDegradation, Reason: "missing_thinking_without_action"}); got != pluginapi.StreamCompletionRetryModeReloadAndExcludeSelectedAuth {
		t.Fatalf("missing thinking retry mode = %q", got)
	}
	if got := realtimeGuardRetryMode(realtimeGuardDecision{Classification: realtimeGuardClassificationDegradation, Reason: "hard_tps"}); got != pluginapi.StreamCompletionRetryModeReloadAndExcludeSelectedAuth {
		t.Fatalf("hard TPS retry mode = %q", got)
	}
	if got := realtimeGuardRetryMode(realtimeGuardDecision{SourceUnavailable: true}); got != pluginapi.StreamCompletionRetryModeReloadAndExcludeSelectedAuth {
		t.Fatalf("unavailable source retry mode = %q", got)
	}

}

func TestRealtimeGuardMutationAuditOmitsSecrets(t *testing.T) {
	decision := realtimeGuardDecision{
		Reason:           "completed_mutation_evidence",
		MutationEvidence: completedMutationEvidence{Matched: true, ScanStartIndex: 1, ToolName: "write_file", CallID: "write-1"},
		OutputTokens:     40,
	}
	audit := realtimeGuardMutationAudit(realtimeGuardProbe{
		RequestID:       "req-test",
		TraceID:         "trace-test",
		AuthIndex:       "auth-1",
		ProxyURL:        "socks5://user:secret-pass@127.0.0.1:1080",
		OriginalRequest: []byte(`{"input":[{"type":"function_call","name":"terminal","arguments":"{\"command\":\"SECRET_COMMAND\"}"}]}`),
	}, decision)
	for _, leaked := range []string{"SECRET_COMMAND", "secret-pass", "socks5://"} {
		if strings.Contains(audit, leaked) {
			t.Fatalf("audit leaked %q: %s", leaked, audit)
		}
	}
	for _, required := range []string{"req-test", "trace-test", "write_file", "write-1", "mutation_scan_start=1"} {
		if !strings.Contains(audit, required) {
			t.Fatalf("audit missing %q: %s", required, audit)
		}
	}
}
