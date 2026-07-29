package main

import (
	"encoding/base64"
	"fmt"
	"strings"
)

func decodeStdBase64(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, fmt.Errorf("empty base64")
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(trimmed)
	if errDecode == nil {
		return decoded, nil
	}
	decoded, errRaw := base64.RawStdEncoding.DecodeString(trimmed)
	if errRaw == nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("base64 decode failed: %v", errDecode)
}
