package auth

import (
	"crypto/sha256"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// BEGIN xAI Guardian core extension: request-scoped evidence for cross-auth wrapup.

type xaiStreamRetryEvidenceContextKey struct{}

// Each streaming execution owns this state. Guard completions are evaluated
// serially; the state never outlives the caller's request or enters the plugin DB.
type xaiStreamRetryEvidence struct {
	authID                string
	authIndex             string
	model                 string
	originalRequestDigest [sha256.Size]byte
	requestBodyDigest     [sha256.Size]byte
}

func (state *xaiStreamRetryEvidence) prepare(completion *cliproxyexecutor.XAIStreamCompletionRequest) {
	metadata := make(map[string]any, len(completion.Metadata)+1)
	for key, value := range completion.Metadata {
		metadata[key] = value
	}
	// Only the host can supply prior-attempt evidence; discard caller/plugin input.
	delete(metadata, pluginapi.XAIStreamPreviousZeroThinkingAuthIndexMetadataKey)
	if state.authID != "" && state.authIndex != "" && completion.AuthID != "" && completion.AuthIndex != "" &&
		state.authID != completion.AuthID && state.authIndex != completion.AuthIndex && state.model == completion.Model &&
		len(completion.OriginalRequest) > 0 && len(completion.RequestBody) > 0 &&
		state.originalRequestDigest == sha256.Sum256(completion.OriginalRequest) &&
		state.requestBodyDigest == sha256.Sum256(completion.RequestBody) {
		metadata[pluginapi.XAIStreamPreviousZeroThinkingAuthIndexMetadataKey] = state.authIndex
	}
	completion.Metadata = metadata
	// Consume the evidence even if the plugin completion callback fails.
	*state = xaiStreamRetryEvidence{}
}

func (state *xaiStreamRetryEvidence) record(completion cliproxyexecutor.XAIStreamCompletionRequest, decision cliproxyexecutor.XAIStreamCompletionResponse) {
	*state = xaiStreamRetryEvidence{}
	if decision.Action != "retry" || decision.Reason != "missing_thinking_without_action" || !decision.ZeroThinkingWithoutAction ||
		!completion.Completed || completion.Error != "" || completion.StatusCode < 200 || completion.StatusCode >= 300 ||
		completion.AuthID == "" || completion.AuthIndex == "" || len(completion.OriginalRequest) == 0 || len(completion.RequestBody) == 0 {
		return
	}
	state.authID = completion.AuthID
	state.authIndex = completion.AuthIndex
	state.model = completion.Model
	state.originalRequestDigest = sha256.Sum256(completion.OriginalRequest)
	state.requestBodyDigest = sha256.Sum256(completion.RequestBody)
}

// END xAI Guardian core extension.
