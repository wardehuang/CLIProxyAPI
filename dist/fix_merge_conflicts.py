from pathlib import Path
import re


def resolve_redisqueue_plugin() -> None:
    path = Path("internal/redisqueue/plugin.go")
    text = path.read_text(encoding="utf-8")
    # Combined struct: upstream accounting fields + local Metadata
    pattern = re.compile(
        r"type queuedUsageDetail struct \{\n"
        r"\trequestDetail\n"
        r"<<<<<<< HEAD\n"
        r".*?"
        r">>>>>>> upstream/main\n"
        r"\}",
        re.S,
    )
    replacement = """type queuedUsageDetail struct {
	requestDetail
	AccountingVersion   int                      `json:"accounting_version"`
	TokenBreakdown      coreusage.TokenBreakdown `json:"token_breakdown"`
	Provider            string                   `json:"provider"`
	ExecutorType        string                   `json:"executor_type"`
	Model               string                   `json:"model"`
	Alias               string                   `json:"alias"`
	Endpoint            string                   `json:"endpoint"`
	AuthType            string                   `json:"auth_type"`
	APIKey              string                   `json:"api_key"`
	RequestID           string                   `json:"request_id"`
	ReasoningEffort     string                   `json:"reasoning_effort"`
	ServiceTier         string                   `json:"service_tier"`
	ResponseServiceTier string                   `json:"response_service_tier,omitempty"`
	Metadata            map[string]any           `json:"metadata,omitempty"`
}"""
    new_text, count = pattern.subn(replacement, text, count=1)
    if count != 1:
        raise SystemExit(f"plugin.go conflict replace failed: {count}")
    # Ensure Metadata is still passed in Marshal
    if "Metadata:            cloneJSONSafeMetadata(record.Metadata)," not in new_text:
        # try re-add if marshal lost it
        if "cloneJSONSafeMetadata" not in new_text:
            raise SystemExit("plugin.go missing metadata clone call")
    path.write_text(new_text, encoding="utf-8")
    print("fixed plugin.go")


def resolve_redisqueue_plugin_test() -> None:
    path = Path("internal/redisqueue/plugin_test.go")
    text = path.read_text(encoding="utf-8")
    pattern = re.compile(
        r"<<<<<<< HEAD\n"
        r"(func TestUsageQueuePluginPayloadIncludesMetadata.*?)\n"
        r"=======\n"
        r"(func TestUsageQueuePluginNormalizesDirectSDKUsageByProvider.*?)\n"
        r">>>>>>> upstream/main\n"
        r"\}",
        re.S,
    )
    m = pattern.search(text)
    if not m:
        # maybe braces differ - broader replace
        pattern2 = re.compile(r"<<<<<<< HEAD\n.*?>>>>>>> upstream/main\n", re.S)
        # extract both sides manually
        start = text.find("<<<<<<< HEAD")
        mid = text.find("=======", start)
        end = text.find(">>>>>>> upstream/main", mid)
        if start < 0 or mid < 0 or end < 0:
            raise SystemExit("plugin_test conflict markers not found")
        head = text[start + len("<<<<<<< HEAD\n") : mid]
        # strip trailing partial brace from head if present
        upstream = text[mid + len("=======\n") : end]
        # both sides end before >>>>>>> ; upstream block may include closing of test
        # Build: both tests fully
        combined = head.rstrip() + "\n\n" + upstream.rstrip() + "\n"
        # if head doesn't end with }\n for last test, fix
        new_text = text[:start] + combined + text[end + len(">>>>>>> upstream/main\n") :]
        # Remove accidental extra } after combined if original had }
        path.write_text(new_text, encoding="utf-8")
        print("fixed plugin_test.go via broad replace")
        return
    head, upstream = m.group(1), m.group(2)
    combined = head.rstrip() + "\n\n" + upstream.rstrip() + "\n"
    new_text = pattern.sub(combined, text, count=1)
    path.write_text(new_text, encoding="utf-8")
    print("fixed plugin_test.go")


def resolve_conductor() -> None:
    path = Path("sdk/cliproxy/auth/conductor.go")
    text = path.read_text(encoding="utf-8")

    # 1) persistCooldownStates conflict -> take upstream structure
    match = re.search(
        r"func \(m \*Manager\) (persist\w+States)\(ctx context\.Context\) \{",
        text,
    )
    if not match:
        raise SystemExit("persist*States not found")
    top = match.group(1)
    start = match.start()
    count_match = re.search(r"func (count\w+RecordsForProvider)\(", text[start:])
    if not count_match:
        raise SystemExit("count*RecordsForProvider not found")
    end = start + count_match.start()
    block = text[start:end]

    lock_match = re.search(r"m\.(config\w+Mu)\.Lock\(\)", block)
    locked_match = re.search(r"func \(m \*Manager\) (persist\w+StatesLocked)\(", block)
    to_locked_match = re.search(r"m\.(persist\w+StatesToLocked)\(", block)
    pending_match = re.search(r"m\.(pending\w+) == store", block)

    if "Cooldown" in top:
        lock_name = "configCooldownMu"
        locked = "persistCooldownStatesLocked"
        to_locked = "persistCooldownStatesToLocked"
        pending = "pendingCooldownStateStore"
    else:
        lock_name = "configDataMu"
        locked = "persistDataStatesLocked"
        to_locked = "persistDataStatesToLocked"
        pending = "pendingDataStateStore"
    if lock_match:
        lock_name = lock_match.group(1)
    if locked_match:
        locked = locked_match.group(1)
    if to_locked_match:
        to_locked = to_locked_match.group(1)
    if pending_match:
        pending = pending_match.group(1)

    replacement = f"""func (m *Manager) {top}(ctx context.Context) {{
	if m == nil {{
		return
	}}
	m.{lock_name}.Lock()
	defer m.{lock_name}.Unlock()
	m.{locked}(ctx)
}}

func (m *Manager) {locked}(ctx context.Context) {{
	m.mu.RLock()
	store := m.cooldownStore
	m.mu.RUnlock()
	if m.{to_locked}(ctx, store) {{
		m.mu.Lock()
		if m.{pending} == store {{
			m.{pending} = nil
		}}
		m.mu.Unlock()
	}}
}}

"""
    text = text[:start] + replacement + text[end:]

    # 2) isXAIQuotaResult vs recordExecutionResult - keep both
    pattern2 = re.compile(
        r"<<<<<<< HEAD\n"
        r"(func isXAIQuotaResult.*?)\n"
        r"=======\n"
        r"(func \(m \*Manager\) recordExecutionResult.*?)\n"
        r">>>>>>> upstream/main\n"
        r"\}",
        re.S,
    )
    m2 = pattern2.search(text)
    if m2:
        combined = m2.group(1).rstrip() + "\n\n" + m2.group(2).rstrip() + "\n"
        text = pattern2.sub(combined, text, count=1)
        print("fixed conductor dual helpers")
    else:
        # broader
        start2 = text.find("<<<<<<< HEAD")
        if start2 >= 0:
            mid2 = text.find("=======", start2)
            end2 = text.find(">>>>>>> upstream/main", mid2)
            head2 = text[start2 + len("<<<<<<< HEAD\n") : mid2]
            up2 = text[mid2 + len("=======\n") : end2]
            combined = head2.rstrip() + "\n\n" + up2.rstrip() + "\n"
            text = text[:start2] + combined + text[end2 + len(">>>>>>> upstream/main\n") :]
            print("fixed conductor dual helpers broad")

    # 3) ensure persistCooldownStatesToLocked uses countCooldownStateRecordsForProvider not Data
    text = text.replace(
        "countDataStateRecordsForProvider(records, \"xai\")",
        "countCooldownStateRecordsForProvider(records, \"xai\")",
    )

    path.write_text(text, encoding="utf-8")
    print("fixed conductor")


def resolve_types() -> None:
    path = Path("sdk/cliproxy/executor/types.go")
    text = path.read_text(encoding="utf-8")
    pattern = re.compile(
        r"<<<<<<< HEAD\n"
        r"\t// RequestFinalizer.*?\n"
        r"\tRequestFinalizer RequestFinalizer\n"
        r"=======\n"
        r"\t// ExecutionLifecycle.*?\n"
        r"\tExecutionLifecycle ExecutionLifecycle\n"
        r">>>>>>> upstream/main\n",
        re.S,
    )
    replacement = (
        "\t// RequestFinalizer runs after provider payload construction and immediately before upstream send.\n"
        "\tRequestFinalizer RequestFinalizer\n"
        "\t// ExecutionLifecycle owns Home-dispatched execution resources. Executors must not add it to request metadata.\n"
        "\tExecutionLifecycle ExecutionLifecycle\n"
    )
    new_text, count = pattern.subn(replacement, text, count=1)
    if count != 1:
        raise SystemExit(f"types.go conflict replace failed: {count}")
    path.write_text(new_text, encoding="utf-8")
    print("fixed types.go")


def resolve_service() -> None:
    path = Path("sdk/cliproxy/service.go")
    text = path.read_text(encoding="utf-8")
    service_match = re.search(
        r"func \(s \*Service\) (resolve\w+StateStore)\(cfg \*config\.Config\) coreauth\.(\w+StateStore) \{\n"
        r"\tif cfg == nil \|\| !cfg\.(\w+) \|\| cfg\.Home\.Enabled \{\n"
        r"<<<<<<< HEAD\n",
        text,
    )
    if not service_match:
        if "<<<<<<<" not in text:
            print("service already clean")
            return
        raise SystemExit("service conflict header not parsed")
    resolve_fn, store_type, save_field = service_match.groups()
    prefix = store_type[: -len("StateStore")]
    auth_fn = f"resolve{prefix}StateAuthDir"
    ctor = f"coreauth.NewFile{prefix}StateStoreWithAuthDir"
    start_s = service_match.start()
    end_match = re.search(r"\nfunc resolve\w+StateAuthDir", text[start_s:])
    if not end_match:
        raise SystemExit("service end not found")
    end_s = start_s + end_match.start() + 1
    replacement_s = f"""func (s *Service) {resolve_fn}(cfg *config.Config) coreauth.{store_type} {{
	if cfg == nil || !cfg.{save_field} || cfg.Home.Enabled {{
		if cfg != nil {{
			log.Infof("XAI_COOLDOWN_TRACE phase=cooldown_store_configuration configured=false save_cooldown_status=%t home_enabled=%t", cfg.{save_field}, cfg.Home.Enabled)
		}}
		return nil
	}}
	authDir, errResolve := {auth_fn}(cfg)
	if errResolve != nil {{
		log.Warnf("failed to resolve cooldown state directory: %v", errResolve)
		return nil
	}}
	if authDir == "" {{
		return nil
	}}
	log.Infof("XAI_COOLDOWN_TRACE phase=cooldown_store_configuration configured=true save_cooldown_status=true home_enabled=false")
	return {ctor}(authDir, authDir)
}}
"""
    path.write_text(text[:start_s] + replacement_s + text[end_s:], encoding="utf-8")
    print("fixed service", resolve_fn, store_type, save_field)


def ensure_request_id_metadata() -> None:
    path = Path("sdk/api/handlers/handlers.go")
    text = path.read_text(encoding="utf-8")
    if 'meta["request_id"]' in text:
        print("request_id already present")
        return
    needle = """\tif requestPath != \"\" {
\t\tmeta[coreexecutor.RequestPathMetadataKey] = requestPath
\t}
"""
    insert = """\tif requestPath != \"\" {
\t\tmeta[coreexecutor.RequestPathMetadataKey] = requestPath
\t}
\t// Expose request_id so request-interceptor plugins can correlate detail logs
\t// with host request-log filenames (e.g. v1-messages-...-<request_id>-strip.log).
\tif requestID := logging.GetRequestID(ctx); requestID != \"\" {
\t\tmeta[\"request_id\"] = requestID
\t} else if ginCtx != nil {
\t\tif requestID := logging.GetGinRequestID(ginCtx); requestID != \"\" {
\t\t\tmeta[\"request_id\"] = requestID
\t\t}
\t}
"""
    if needle not in text:
        raise SystemExit("handlers.go request_id insertion point not found")
    path.write_text(text.replace(needle, insert, 1), encoding="utf-8")
    print("inserted request_id metadata")


def main() -> None:
    resolve_redisqueue_plugin()
    resolve_redisqueue_plugin_test()
    resolve_conductor()
    resolve_types()
    resolve_service()
    ensure_request_id_metadata()

    for rel in [
        "internal/redisqueue/plugin.go",
        "internal/redisqueue/plugin_test.go",
        "sdk/cliproxy/auth/conductor.go",
        "sdk/cliproxy/executor/types.go",
        "sdk/cliproxy/service.go",
        "sdk/api/handlers/handlers.go",
    ]:
        markers = Path(rel).read_text(encoding="utf-8").count("<<<<<<<")
        print(f"{rel}: markers={markers}")
        if markers:
            raise SystemExit(f"still has markers: {rel}")


if __name__ == "__main__":
    main()
