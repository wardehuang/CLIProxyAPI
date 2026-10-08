package auth

import (
	"encoding/json"
	"testing"
)

func TestSchedulerAuthCandidatesIncludeScheduleGroupFromMetadata(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{
			name: "top-level numeric value",
			metadata: map[string]any{
				"schedule_group": float64(2),
				"access_token":   "must-not-be-copied",
			},
			want: "2",
		},
		{
			name:     "nested attributes value",
			metadata: map[string]any{"attributes": map[string]any{"schedule_group": "3"}},
			want:     "3",
		},
		{
			name:     "nested metadata number",
			metadata: map[string]any{"metadata": map[string]any{"schedule_group": json.Number("4")}},
			want:     "4",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates := schedulerAuthCandidates([]*Auth{{Provider: "xai", Metadata: test.metadata}})
			if len(candidates) != 1 {
				t.Fatalf("candidate count = %d, want 1", len(candidates))
			}
			if got := candidates[0].Attributes["schedule_group"]; got != test.want {
				t.Fatalf("schedule_group = %q, want %q", got, test.want)
			}
			if _, ok := candidates[0].Attributes["access_token"]; ok {
				t.Fatal("sensitive metadata was copied to scheduler attributes")
			}
		})
	}
}
