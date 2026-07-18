package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestShouldDemoteXAIAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		record pluginapi.UsageRecord
		want   bool
	}{
		{
			name: "xai 403 failed",
			record: pluginapi.UsageRecord{
				Provider: "xai",
				Failed:   true,
				Failure:  pluginapi.UsageFailure{StatusCode: http.StatusForbidden},
			},
			want: true,
		},
		{
			name: "provider case insensitive",
			record: pluginapi.UsageRecord{
				Provider: "XAI",
				Failed:   true,
				Failure:  pluginapi.UsageFailure{StatusCode: http.StatusForbidden},
			},
			want: true,
		},
		{
			name: "not failed",
			record: pluginapi.UsageRecord{
				Provider: "xai",
				Failed:   false,
				Failure:  pluginapi.UsageFailure{StatusCode: http.StatusForbidden},
			},
			want: false,
		},
		{
			name: "other status",
			record: pluginapi.UsageRecord{
				Provider: "xai",
				Failed:   true,
				Failure:  pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
			},
			want: false,
		},
		{
			name: "other provider",
			record: pluginapi.UsageRecord{
				Provider: "codex",
				Failed:   true,
				Failure:  pluginapi.UsageFailure{StatusCode: http.StatusForbidden},
			},
			want: false,
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldDemoteXAIAuth(testCase.record); got != testCase.want {
				t.Fatalf("shouldDemoteXAIAuth() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestSetAuthPriorityJSON(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"xai","email":"a@b.com","priority":5,"access_token":"tok"}`)
	updated, previous, alreadyTarget, errPatch := setAuthPriorityJSON(raw, targetPriorityValue)
	if errPatch != nil {
		t.Fatalf("setAuthPriorityJSON() error: %v", errPatch)
	}
	if alreadyTarget {
		t.Fatal("alreadyTarget = true, want false")
	}
	if previous != "5" {
		t.Fatalf("previous = %q, want 5", previous)
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(updated, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal updated json: %v", errUnmarshal)
	}
	if got := int(payload["priority"].(float64)); got != targetPriorityValue {
		t.Fatalf("priority = %d, want %d", got, targetPriorityValue)
	}
	if payload["email"] != "a@b.com" {
		t.Fatalf("email = %v, want a@b.com", payload["email"])
	}
	if payload["access_token"] != "tok" {
		t.Fatalf("access_token lost")
	}
}

func TestSetAuthPriorityJSONAlreadyTarget(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"xai","priority":-1}`)
	_, previous, alreadyTarget, errPatch := setAuthPriorityJSON(raw, targetPriorityValue)
	if errPatch != nil {
		t.Fatalf("setAuthPriorityJSON() error: %v", errPatch)
	}
	if !alreadyTarget {
		t.Fatal("alreadyTarget = false, want true")
	}
	if previous != "-1" {
		t.Fatalf("previous = %q, want -1", previous)
	}
}
