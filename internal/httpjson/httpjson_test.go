package httpjson

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseFailuresBeforeHeaders(t *testing.T) {
	t.Parallel()
	for _, value := range []any{make(chan int), strings.Repeat("x", MaxResponseBytes)} {
		w := httptest.NewRecorder()
		Write(w, 201, value)
		if w.Code != 500 || !strings.Contains(w.Body.String(), `"code":"internal_error"`) || w.Body.Len() > 256 {
			t.Fatalf("partial success: %d %s", w.Code, w.Body.String())
		}
	}
}
