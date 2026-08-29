# CPA Realtime Guard isRealThinking Implementation Plan

> **For Hermes:** Execute directly in the current workspace; do not commit, test, or deploy without explicit user instruction.

**Goal:** Replace the realtime guard's event-presence-only `thinkingDelta` decision with configurable, evidence-based `isRealThinking` classification.

**Architecture:** Parse real xAI Responses SSE fields already present in CPA: reasoning summary deltas/done text, reasoning output items, `encrypted_content`, completed usage, visible output, and function calls. Build one `isRealThinking` Boolean. Keep hard TPS unconditional; only the soft TPS rule becomes `softTPS < TPS < hardTPS && !isRealThinking`.

**Tech Stack:** Go plugin, SQLite plugin settings, embedded HTML/JavaScript WebUI.

---

## Confirmed semantics

- Seven configurable thresholds:
  - minimum reasoning summary characters: `32`
  - minimum encrypted content bytes: `256`
  - encrypted bytes per reasoning token: `4`
  - minimum output tokens before checking: `8`
  - burst minimum reasoning tokens: `80`
  - burst maximum visible tokens: `32`
  - burst maximum visible flush window: `1000ms`
- Below minimum output tokens: skip authenticity evaluation and set `isRealThinking=true`.
- Core evidence: non-empty summary text meeting its threshold OR completed reasoning item whose encrypted bytes meet `max(minEncryptedBytes, reasoningTokens * bytesPerReasoningToken)`.
- Burst evidence invalidates otherwise-real thinking.
- Hard TPS behavior stays unchanged.
- Existing `thinkingDelta` may remain as diagnostic evidence, but must not drive soft TPS classification.

## Task 1: Trace and extend plugin settings

**Files:**
- Modify: `plugins/src/cpa-xai-ip-switcher/store.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/slots.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/main.go`
- Modify: `plugins/src/cpa-xai-ip-switcher/runtime.go`

**Steps:**
1. Add seven defaults and `pluginSettings` fields.
2. Add SQLite defaults/migration rows.
3. Add settings SELECT parsing and save map entries.
4. Add backend validation with explicit ranges.
5. Add payload parsing and public settings response fields.
6. Add equality checks and settings-updated diagnostics.

## Task 2: Implement real-thinking evidence parser

**Files:**
- Modify: `plugins/src/cpa-xai-ip-switcher/realtime_guard.go`

**Steps:**
1. Replace the old tuple return from `parseRealtimeGuardSSE` with a structured evidence result.
2. Parse real event fields:
   - `response.reasoning_summary_text.delta.delta`
   - `response.reasoning_summary_text.done.text`
   - `response.output_item.added.item.id/type/status`
   - `response.output_item.done.item.id/type/status/summary/encrypted_content`
   - `response.completed.response.output`
   - `response.completed.response.usage.output_tokens`
   - `response.completed.response.usage.output_tokens_details.reasoning_tokens`
   - `response.output_text.delta.delta`
   - completed `function_call` items
3. Check reasoning item ID consistency and completion.
4. Compute summary characters, encrypted byte floor, visible tokens, semantic output, and burst evidence.
5. Produce `isRealThinking` and a diagnostic reason.
6. Keep minimum-output bypass exactly as confirmed.
7. Replace only the soft TPS condition with `!isRealThinking`.

## Task 3: Add WebUI controls and detailed descriptions

**Files:**
- Modify: `plugins/src/cpa-xai-ip-switcher/page.html`

**Steps:**
1. Add seven numeric inputs to the realtime guard settings card.
2. State units, defaults, comparison directions, and the complete Boolean formula.
3. Register elements, refill saved values on page load, include values in save payload, and validate ranges client-side.
4. Preserve the existing dark-theme control style and immediate-effect wording.

## Task 4: Static/build verification

**Commands:**
- `gofmt -w` on changed Go files.
- `git diff --check`.
- `go build -o NUL ./cmd/server`.
- Build the plugin only if a local CGO-capable toolchain exists; otherwise report it as unverified.

**Explicit omissions:**
- Do not run tests.
- Do not commit.
- Do not deploy.

## Risks

- SSE summary text can be repeated in delta, done, output-item-done, and completed payloads; use canonical/max evidence rather than double-counting.
- Tool-only turns have zero visible text but remain semantic output; do not classify them as empty.
- `encrypted_content` is opaque and cannot prove semantic quality; length only provides consistency evidence.
- Accurate burst timing requires event receive timestamps. If the current completion contract exposes only request/first-payload/finish times, use the available generation window and document the limitation rather than inventing timing fields.
