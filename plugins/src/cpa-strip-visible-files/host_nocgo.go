//go:build !cgo

package main

import (
	"encoding/json"
	"fmt"
)

func callHost(method string, payload any) (json.RawMessage, error) {
	return nil, fmt.Errorf("host callback %s is unavailable without cgo", method)
}
