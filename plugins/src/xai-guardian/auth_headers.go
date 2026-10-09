package main

import (
	"fmt"
	"net/http"
	"strings"
)

func applyXAIAuthJSONHeaders(request *http.Request, rawAuth map[string]any) error {
	rawHeaders, exists := rawAuth["headers"]
	if !exists || rawHeaders == nil {
		return nil
	}
	headers, ok := rawHeaders.(map[string]any)
	if !ok {
		return fmt.Errorf("xAI auth headers must be an object")
	}
	for name, rawValue := range headers {
		headerName := strings.TrimSpace(name)
		if headerName == "" || rawValue == nil {
			continue
		}
		headerValue, ok := rawValue.(string)
		if !ok {
			return fmt.Errorf("xAI auth header %q must be a string", headerName)
		}
		headerValue = strings.TrimSpace(headerValue)
		if headerValue == "" {
			continue
		}
		if strings.HasPrefix(headerValue, "$") || strings.Contains(strings.ToUpper(headerValue), "$CPA-SESSION-ID") {
			return fmt.Errorf("xAI auth header %q requires request context", headerName)
		}
		if http.CanonicalHeaderKey(headerName) == "Host" {
			request.Host = headerValue
		}
		request.Header.Set(headerName, headerValue)
	}
	return nil
}
