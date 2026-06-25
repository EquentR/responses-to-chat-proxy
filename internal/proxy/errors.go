package proxy

import "fmt"

func errorPayload(message, errorType, code string, param ...string) map[string]any {
	errorBody := map[string]any{
		"message": message,
		"type":    errorType,
		"code":    code,
	}
	if len(param) > 0 && param[0] != "" {
		errorBody["param"] = param[0]
	}
	return map[string]any{"error": errorBody}
}

func wrapUpstreamError(data any, statusCode int) map[string]any {
	original := map[string]any{"raw": fmt.Sprint(data)}
	if dict, ok := data.(map[string]any); ok {
		original = dict
	}

	message := "Upstream error"
	errorType := "upstream_error"
	code := fmt.Sprintf("http_%d", statusCode)

	if dict, ok := data.(map[string]any); ok {
		if upstreamError, ok := dict["error"].(map[string]any); ok {
			if value := stringValue(upstreamError["message"]); value != "" {
				message = value
			}
			if value := stringValue(upstreamError["type"]); value != "" {
				errorType = value
			}
			if value := stringValue(upstreamError["code"]); value != "" {
				code = value
			}
		} else if value := stringValue(dict["error"]); value != "" {
			message = value
		} else if value := stringValue(dict["detail"]); value != "" {
			message = value
		} else if value := stringValue(dict["message"]); value != "" {
			message = value
		} else if value := stringValue(dict["raw_response"]); value != "" {
			message = trimMessage(value)
		}
	} else {
		message = trimMessage(fmt.Sprint(data))
	}

	return map[string]any{
		"error": map[string]any{
			"message":           message,
			"type":              errorType,
			"code":              code,
			"original_response": original,
		},
	}
}

func sseError(message, code string, errorType ...string) []byte {
	typ := "upstream_error"
	if len(errorType) > 0 && errorType[0] != "" {
		typ = errorType[0]
	}
	return []byte(sseEvent("error", map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"code":    code,
		},
	}))
}

func trimMessage(message string) string {
	if len(message) > 500 {
		return message[:500]
	}
	return message
}
