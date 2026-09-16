package httpjson

import (
	"encoding/json"
	"net/http"
)

const MaxResponseBytes = 4 << 20

// Write encodes before committing headers, including the response size check.
func Write(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil || len(body)+1 > MaxResponseBytes {
		Error(w, http.StatusInternalServerError, "internal_error", "response could not be encoded within the size limit")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// outer response writer records transport failure without logging raw errors
	_, _ = w.Write(append(body, '\n'))
}

func Error(w http.ResponseWriter, status int, code, message string) {
	Write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func NotFound(w http.ResponseWriter) {
	Error(w, http.StatusNotFound, "not_found", "not found")
}

func MethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
