package provider

import (
	"errors"
	"net/http"
	"strings"

	"github.com/openai/openai-go"
)

// CapabilityErrorKind categorizes LLM gateway parameter and protocol capability rejections.
type CapabilityErrorKind int

// Capability error classifications.
const (
	// CapabilityErrorNone indicates no capability error detected.
	CapabilityErrorNone CapabilityErrorKind = iota
	// CapabilityErrorEndpointUnsupported indicates an HTTP 404/405 (e.g. /v1/responses not found).
	CapabilityErrorEndpointUnsupported
	// CapabilityErrorReasoningConflict indicates gateway rejection of tools with reasoning effort on /v1/chat/completions.
	CapabilityErrorReasoningConflict
	// CapabilityErrorParamRejected indicates generic rejection of parameters like reasoning_effort.
	CapabilityErrorParamRejected
)

// ParseCapabilityError analyzes API error responses to determine protocol or parameter capability issues.
func ParseCapabilityError(err error) CapabilityErrorKind {
	if err == nil {
		return CapabilityErrorNone
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed {
			return CapabilityErrorEndpointUnsupported
		}
		if apiErr.StatusCode == http.StatusBadRequest {
			msg := strings.ToLower(apiErr.Message)
			if strings.Contains(msg, "reasoning_effort") &&
				(strings.Contains(msg, "function tools") ||
					strings.Contains(msg, "responses") ||
					strings.Contains(msg, "not supported")) {
				return CapabilityErrorReasoningConflict
			}
			if strings.Contains(msg, "unrecognized parameter") ||
				strings.Contains(msg, "extra_forbidden") ||
				strings.Contains(msg, "unknown parameter") {
				return CapabilityErrorParamRejected
			}
		}
	}

	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") || strings.Contains(errStr, "405") {
		return CapabilityErrorEndpointUnsupported
	}
	if strings.Contains(errStr, "reasoning_effort") &&
		(strings.Contains(errStr, "function tools") ||
			strings.Contains(errStr, "responses") ||
			strings.Contains(errStr, "not supported")) {
		return CapabilityErrorReasoningConflict
	}
	if strings.Contains(errStr, "unrecognized parameter") || strings.Contains(errStr, "extra_forbidden") {
		return CapabilityErrorParamRejected
	}

	return CapabilityErrorNone
}
