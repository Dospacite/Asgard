package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesSPAEntryWithoutRedirect(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"/", "/login", "/projects/example"} {
		target := target
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, target, nil)
			response := httptest.NewRecorder()

			Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d, want 200", target, response.Code)
			}
			if location := response.Header().Get("Location"); location != "" {
				t.Fatalf("GET %s redirected to %q", target, location)
			}
			if !strings.Contains(response.Body.String(), `<div id="root"></div>`) {
				t.Fatalf("GET %s did not serve the SPA entry point", target)
			}
		})
	}
}
