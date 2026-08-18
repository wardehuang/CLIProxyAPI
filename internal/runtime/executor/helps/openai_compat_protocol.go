package helps

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	codexopenai "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/openai/chat-completions"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAICompatWire describes the upstream protocol selected for an openai-compatibility provider.
type OpenAICompatWire struct {
	Format   sdktranslator.Format
	Endpoint string
	UsesResp bool
}

// ResolveOpenAICompatWire returns the upstream format and path for openai-compat chat traffic.
// Compact and image endpoints keep their existing special cases outside this helper.
func ResolveOpenAICompatWire(protocol string, alt string) OpenAICompatWire {
	if strings.TrimSpace(alt) == "responses/compact" {
		return OpenAICompatWire{
			Format:   sdktranslator.FormatOpenAIResponse,
			Endpoint: "/responses/compact",
			UsesResp: true,
		}
	}
	if config.OpenAICompatibilityUsesResponses(protocol) {
		return OpenAICompatWire{
			Format:   sdktranslator.FormatOpenAIResponse,
			Endpoint: "/responses",
			UsesResp: true,
		}
	}
	return OpenAICompatWire{
		Format:   sdktranslator.FormatOpenAI,
		Endpoint: "/chat/completions",
		UsesResp: false,
	}
}

// TranslateOpenAICompatRequest converts a client payload into the selected openai-compat upstream schema.
// When the upstream speaks Responses and no direct transformer exists, the request hops through chat completions.
func TranslateOpenAICompatRequest(ctx context.Context, headers http.Header, cfg *config.Config, from, to sdktranslator.Format, model string, payload []byte, stream, isCompat bool) []byte {
	if to != sdktranslator.FormatOpenAIResponse {
		return TranslateRequestWithAPIKeyModelCompatibility(ctx, headers, cfg, from, to, model, payload, stream, isCompat)
	}
	if from == sdktranslator.FormatOpenAIResponse || from == "" {
		return TranslateRequestWithAPIKeyModelCompatibility(ctx, headers, cfg, sdktranslator.FormatOpenAIResponse, to, model, payload, stream, isCompat)
	}
	if sdktranslator.HasRequestTransformer(from, to) {
		return TranslateRequestWithAPIKeyModelCompatibility(ctx, headers, cfg, from, to, model, payload, stream, isCompat)
	}

	chatPayload := payload
	if from != sdktranslator.FormatOpenAI {
		chatPayload = TranslateRequestWithAPIKeyModelCompatibility(ctx, headers, cfg, from, sdktranslator.FormatOpenAI, model, payload, stream, isCompat)
	}
	return ConvertOpenAIChatCompletionsRequestToResponses(model, chatPayload, stream)
}

// ConvertOpenAIChatCompletionsRequestToResponses converts a chat-completions JSON body into Responses API JSON.
// It reuses the mature Codex chat→responses mapping, then strips Codex-only defaults and restores common generation fields.
func ConvertOpenAIChatCompletionsRequestToResponses(modelName string, chatPayload []byte, stream bool) []byte {
	out := codexopenai.ConvertOpenAIRequestToCodex(modelName, chatPayload, stream)
	out, _ = sjson.DeleteBytes(out, "include")

	chat := gjson.ParseBytes(chatPayload)
	if !chat.Get("parallel_tool_calls").Exists() {
		out, _ = sjson.DeleteBytes(out, "parallel_tool_calls")
	} else {
		out, _ = sjson.SetBytes(out, "parallel_tool_calls", chat.Get("parallel_tool_calls").Bool())
	}
	if !chat.Get("reasoning_effort").Exists() && !chat.Get("reasoning").Exists() {
		out, _ = sjson.DeleteBytes(out, "reasoning")
	}
	if v := chat.Get("temperature"); v.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", v.Value())
	}
	if v := chat.Get("top_p"); v.Exists() {
		out, _ = sjson.SetBytes(out, "top_p", v.Value())
	}
	if v := chat.Get("max_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	} else if v := chat.Get("max_completion_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	}
	if v := chat.Get("prompt_cache_key"); v.Exists() && v.Type == gjson.String {
		out = SetStringIfDifferent(out, "prompt_cache_key", v.String())
	}
	if v := chat.Get("user"); v.Exists() && v.Type == gjson.String {
		out = SetStringIfDifferent(out, "user", v.String())
	}
	if v := chat.Get("metadata"); v.Exists() {
		out, _ = sjson.SetRawBytes(out, "metadata", []byte(v.Raw))
	}
	if v := chat.Get("service_tier"); v.Exists() && v.Type == gjson.String {
		out = SetStringIfDifferent(out, "service_tier", v.String())
	}
	if v := chat.Get("store"); v.Exists() {
		out, _ = sjson.SetBytes(out, "store", v.Bool())
	}
	// response_format is already mapped by ConvertOpenAIRequestToCodex into text.format.
	out = SetBoolIfDifferent(out, "stream", stream)
	if strings.TrimSpace(gjson.GetBytes(out, "instructions").String()) == "" {
		out, _ = sjson.DeleteBytes(out, "instructions")
	}
	return out
}

type openAICompatStreamHopState struct {
	upstream any
	client   any
}

// TranslateOpenAICompatNonStreamResponse converts an upstream openai-compat response into the client schema.
func TranslateOpenAICompatNonStreamResponse(ctx context.Context, from, to sdktranslator.Format, model string, originalRequest, translatedRequest, body []byte) []byte {
	if to == "" || from == to {
		if to == sdktranslator.FormatOpenAIResponse {
			return EnsureResponsesUsageDetails(body)
		}
		return body
	}
	if from != sdktranslator.FormatOpenAIResponse {
		out := sdktranslator.TranslateNonStream(ctx, from, to, model, originalRequest, translatedRequest, body, nil)
		if to == sdktranslator.FormatOpenAIResponse {
			out = EnsureResponsesUsageDetails(out)
		}
		return out
	}
	if sdktranslator.HasNonStreamResponseTransformer(to, from) {
		out := sdktranslator.TranslateNonStream(ctx, from, to, model, originalRequest, translatedRequest, body, nil)
		if to == sdktranslator.FormatOpenAIResponse {
			out = EnsureResponsesUsageDetails(out)
		}
		return out
	}

	chatBody := ConvertOpenAIResponsesObjectToChatCompletionsNonStream(ctx, model, originalRequest, translatedRequest, body)
	if len(chatBody) == 0 {
		return body
	}
	if to == sdktranslator.FormatOpenAI {
		return chatBody
	}
	return sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, to, model, originalRequest, translatedRequest, chatBody, nil)
}

// TranslateOpenAICompatStreamResponse converts one upstream SSE payload into zero or more client frames.
func TranslateOpenAICompatStreamResponse(ctx context.Context, from, to sdktranslator.Format, model string, originalRequest, translatedRequest, streamLine []byte, param *any) [][]byte {
	if to == "" || from == to {
		if to == sdktranslator.FormatOpenAIResponse {
			return [][]byte{EnsureResponsesUsageDetails(streamLine)}
		}
		return [][]byte{streamLine}
	}
	if from != sdktranslator.FormatOpenAIResponse {
		return sdktranslator.TranslateStream(ctx, from, to, model, originalRequest, translatedRequest, streamLine, param)
	}
	if sdktranslator.HasStreamResponseTransformer(to, from) {
		return sdktranslator.TranslateStream(ctx, from, to, model, originalRequest, translatedRequest, streamLine, param)
	}

	state := ensureOpenAICompatStreamHopState(param)
	chatFrames := codexopenai.ConvertCodexResponseToOpenAI(ctx, model, originalRequest, translatedRequest, streamLine, &state.upstream)
	if to == sdktranslator.FormatOpenAI {
		return chatFrames
	}
	out := make([][]byte, 0, len(chatFrames))
	for _, frame := range chatFrames {
		// ConvertCodexResponseToOpenAI returns bare JSON objects; wrap them as SSE data lines
		// so the OpenAI→client stream translators see the same shape as native chat upstreams.
		chatLine := frame
		if !strings.HasPrefix(strings.TrimSpace(string(frame)), "data:") {
			chatLine = append([]byte("data: "), frame...)
		}
		out = append(out, sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, to, model, originalRequest, translatedRequest, chatLine, &state.client)...)
	}
	return out
}

// ConvertOpenAIResponsesObjectToChatCompletionsNonStream converts a Responses JSON object
// (or a response.completed envelope) into a chat.completion object.
func ConvertOpenAIResponsesObjectToChatCompletionsNonStream(ctx context.Context, model string, originalRequest, translatedRequest, body []byte) []byte {
	root := gjson.ParseBytes(body)
	switch {
	case root.Get("type").String() == "response.completed" || root.Get("type").String() == "response.incomplete":
		return codexopenai.ConvertCodexResponseToOpenAINonStream(ctx, model, originalRequest, translatedRequest, body, nil)
	case root.Get("object").String() == "response" || root.Get("output").Exists():
		envelope := []byte(`{"type":"response.completed"}`)
		status := strings.TrimSpace(root.Get("status").String())
		if status == "incomplete" {
			envelope = []byte(`{"type":"response.incomplete"}`)
		}
		envelope, _ = sjson.SetRawBytes(envelope, "response", body)
		return codexopenai.ConvertCodexResponseToOpenAINonStream(ctx, model, originalRequest, translatedRequest, envelope, nil)
	default:
		return body
	}
}

func ensureOpenAICompatStreamHopState(param *any) *openAICompatStreamHopState {
	if param == nil {
		state := &openAICompatStreamHopState{}
		return state
	}
	if *param == nil {
		state := &openAICompatStreamHopState{}
		*param = state
		return state
	}
	if state, ok := (*param).(*openAICompatStreamHopState); ok && state != nil {
		return state
	}
	state := &openAICompatStreamHopState{}
	*param = state
	return state
}
