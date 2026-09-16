package executor

// RequestFinalizerMetadataKeys identifies metadata owned by one upstream attempt.
// The list includes explicitly cleared keys so their deletion survives map copies.
const RequestFinalizerMetadataKeys = "request_finalizer_metadata_keys"

// ClearRequestFinalizerMetadata removes the previous attempt's finalizer output.
func ClearRequestFinalizerMetadata(metadata map[string]any) {
	if raw, exists := metadata[RequestFinalizerMetadataKeys]; exists {
		for _, key := range raw.([]string) {
			delete(metadata, key)
		}
	}
	delete(metadata, RequestFinalizerMetadataKeys)
}

// ApplyRequestFinalizerMetadata records both values and deletions for this attempt.
func ApplyRequestFinalizerMetadata(metadata, updates map[string]any, clear []string) {
	if metadata == nil {
		return
	}
	keys := make([]string, 0, len(updates)+len(clear))
	for _, key := range clear {
		delete(metadata, key)
		if _, updated := updates[key]; !updated {
			keys = append(keys, key)
		}
	}
	for key, value := range updates {
		metadata[key] = value
		keys = append(keys, key)
	}
	metadata[RequestFinalizerMetadataKeys] = keys
}

// CopyRequestFinalizerMetadata copies attempt-owned values into a distinct map.
// Callers clear the destination's previous attempt before merging a new one.
func CopyRequestFinalizerMetadata(destination, source map[string]any) {
	if raw, exists := source[RequestFinalizerMetadataKeys]; exists {
		keys := raw.([]string)
		for _, key := range keys {
			if value, present := source[key]; present {
				destination[key] = value
			} else {
				delete(destination, key)
			}
		}
		destination[RequestFinalizerMetadataKeys] = append([]string(nil), keys...)
	}
}
