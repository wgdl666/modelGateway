package worker

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"unicode"
)

// Preserve only bounded validation fields, never raw response bodies, HTML,
// authentication errors or server failures. Unwrap keeps status/retry behavior.
type modelValidationError struct {
	status  modelStatusError
	message string
}

func (e *modelValidationError) Error() string { return e.status.Error() }
func (e *modelValidationError) Unwrap() error { return e.status }

func readModelValidationError(resp *http.Response) *modelValidationError {
	switch resp.StatusCode {
	case 400, 413, 415, 422:
	default:
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil
	}
	raw, err := readModelBody(resp.Body, 16<<10)
	if err != nil {
		return nil
	}
	var body struct {
		Error struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	message := cleanValidationText(body.Error.Message, 2048)
	if message == "" {
		message = cleanValidationText(body.Detail, 2048)
	}
	if message == "" {
		return nil
	}
	if field := cleanValidationText(body.Error.Field, 128); field != "" {
		message = field + ": " + message
	}
	return &modelValidationError{status: modelStatusError(resp.StatusCode), message: message}
}

func cleanValidationText(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value))
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func modelValidationMessage(err error, fallback string) string {
	var detail *modelValidationError
	if errors.As(err, &detail) {
		return detail.message
	}
	return fallback
}
