package redisqueue

import (
	"encoding/json"
	"reflect"
)

// cloneJSONSafeMetadata copies request metadata for usage queue payloads.
// Values that cannot be JSON-encoded (func, chan, complex, nested unsupported types)
// are dropped so a single non-serializable field cannot discard the whole usage event.
func cloneJSONSafeMetadata(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	destination := make(map[string]any, len(source))
	for key, value := range source {
		clonedValue, isSafe := cloneJSONSafeValue(value)
		if !isSafe {
			continue
		}
		destination[key] = clonedValue
	}
	if len(destination) == 0 {
		return nil
	}
	return destination
}

func cloneJSONSafeValue(value any) (any, bool) {
	if value == nil {
		return nil, true
	}
	if !isJSONSafeKind(reflect.ValueOf(value)) {
		return nil, false
	}
	encoded, encodeError := json.Marshal(value)
	if encodeError != nil {
		return nil, false
	}
	var decoded any
	if decodeError := json.Unmarshal(encoded, &decoded); decodeError != nil {
		return nil, false
	}
	return decoded, true
}

func isJSONSafeKind(value reflect.Value) bool {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return true
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128, reflect.Uintptr:
		return false
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return false
		}
		for _, mapKey := range value.MapKeys() {
			if !isJSONSafeKind(value.MapIndex(mapKey)) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if !isJSONSafeKind(value.Index(index)) {
				return false
			}
		}
		return true
	default:
		return true
	}
}
