package main

import (
	"encoding/json"
	"testing"
)

func mutationRequestForTest(t *testing.T, items ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"input": items})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mutationCallForTest(tool, callID string) map[string]any {
	return map[string]any{"type": "function_call", "name": tool, "call_id": callID, "arguments": "{}"}
}

func mutationOutputForTest(callID, output string) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}
}

func mutationEvidenceCasesForTest(t *testing.T) []struct {
	name    string
	request []byte
	want    completedMutationEvidence
} {
	t.Helper()
	user := map[string]any{"role": "user", "content": "Update the implementation."}
	assistant := map[string]any{"role": "assistant", "content": "The previous task is complete."}
	patch := mutationCallForTest("patch", "edit-1")
	success := mutationOutputForTest("edit-1", `{"success":true}`)
	cases := []struct {
		name    string
		request []byte
		want    completedMutationEvidence
	}{
		{"malformed request", []byte("{"), completedMutationEvidence{ScanStartIndex: -1}},
		{"string input", []byte(`{"input":"Update the implementation."}`), completedMutationEvidence{ScanStartIndex: -1}},
		{"no user", mutationRequestForTest(t, patch, success), completedMutationEvidence{ScanStartIndex: -1}},
		{"empty current turn", mutationRequestForTest(t, user), completedMutationEvidence{ScanStartIndex: 1}},
		{"independent previous task", mutationRequestForTest(t, user, patch, success, assistant, user), completedMutationEvidence{ScanStartIndex: 5}},
		{"adjacent user continuation", mutationRequestForTest(t, user, patch, success, user), completedMutationEvidence{Matched: true, ScanStartIndex: 1, ToolName: "patch", CallID: "edit-1"}},
		{"output without call", mutationRequestForTest(t, user, success), completedMutationEvidence{ScanStartIndex: 1}},
		{"call without output", mutationRequestForTest(t, user, patch), completedMutationEvidence{ScanStartIndex: 1}},
		{"different call id", mutationRequestForTest(t, user, patch, mutationOutputForTest("other", `{"success":true}`)), completedMutationEvidence{ScanStartIndex: 1}},
		{"output before call", mutationRequestForTest(t, user, success, patch), completedMutationEvidence{ScanStartIndex: 1}},
		{"call before current user", mutationRequestForTest(t, user, patch, assistant, user, success), completedMutationEvidence{ScanStartIndex: 4}},
	}
	for _, tool := range []string{"patch", "write_file", "skill_manage"} {
		result := `{"success":true}`
		wrongField := `{"verified":true}`
		if tool == "write_file" {
			result, wrongField = wrongField, result
		}
		for _, tc := range []struct {
			name, output, callID string
			matched              bool
		}{
			{"success", result, "edit-1", true},
			{"failed", `{"success":false,"verified":false}`, "edit-1", false},
			{"missing field", `{}`, "edit-1", false},
			{"wrong field", wrongField, "edit-1", false},
			{"null field", `{"success":null,"verified":null}`, "edit-1", false},
			{"wrong type", `{"success":"true","verified":"true"}`, "edit-1", false},
			{"malformed output", `{`, "edit-1", false},
			{"missing call id", result, "", false},
		} {
			want := completedMutationEvidence{ScanStartIndex: 1}
			if tc.matched {
				want.Matched, want.ToolName, want.CallID = true, tool, tc.callID
			}
			cases = append(cases, struct {
				name    string
				request []byte
				want    completedMutationEvidence
			}{
				tool + " " + tc.name,
				mutationRequestForTest(t, user, mutationCallForTest(tool, tc.callID), mutationOutputForTest(tc.callID, tc.output)), want,
			})
		}
	}
	for _, command := range []string{"SELECT 1", "go env", "printf changed > example.txt"} {
		call := mutationCallForTest("terminal", "terminal-1")
		args, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		call["arguments"] = string(args)
		cases = append(cases, struct {
			name    string
			request []byte
			want    completedMutationEvidence
		}{
			"terminal " + command,
			mutationRequestForTest(t, user, call, mutationOutputForTest("terminal-1", `{"exit_code":0,"error":null,"success":true,"verified":true}`)),
			completedMutationEvidence{ScanStartIndex: 1},
		})
	}
	return cases
}

func TestScanCompletedMutationEvidence(t *testing.T) {
	for _, tc := range mutationEvidenceCasesForTest(t) {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanCompletedMutationEvidence(tc.request); got != tc.want {
				t.Fatalf("evidence = %+v, want %+v", got, tc.want)
			}
		})
	}
}
