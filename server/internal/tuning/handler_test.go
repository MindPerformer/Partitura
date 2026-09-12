package tuning

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateRejectsInvalidParametersWithBadRequest(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/tuning/validate", strings.NewReader(`{"parameters":{"rrf_k":201}}`))
	w := httptest.NewRecorder()
	h.Validate(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

func TestFailMapsConflictAndNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"conflict", ErrConflict, http.StatusConflict},
		{"not_found", sql.ErrNoRows, http.StatusNotFound},
		{"invalid", fmt.Errorf("%w: bad", ErrInvalidParameter), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil, nil, nil)
			w := httptest.NewRecorder()
			h.fail(w, tc.err, tc.name)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d", w.Code, tc.want)
			}
		})
	}
}
