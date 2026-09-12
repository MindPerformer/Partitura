package tuning

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"partitura/server/internal/auth"
	"partitura/server/internal/profile"
)

type conflictTuningRepo struct{}

func (conflictTuningRepo) GetState(context.Context, string) (*State, error) {
	return &State{Draft: Parameters{}, Published: Parameters{}, History: []Release{}}, nil
}
func (conflictTuningRepo) SaveDraft(context.Context, string, int64, Parameters, string) (*State, error) {
	return nil, ErrConflict
}
func (conflictTuningRepo) Publish(context.Context, string, int64, string) (*State, error) {
	return nil, ErrConflict
}
func (conflictTuningRepo) Rollback(context.Context, string, int64, int64, string) (*State, error) {
	return nil, ErrConflict
}
func (conflictTuningRepo) Clear(context.Context, string, int64, string) (*State, error) {
	return nil, ErrConflict
}
func (conflictTuningRepo) Effective(context.Context, string) (*profile.ProfileRecord, Parameters, *State, error) {
	return nil, nil, nil, ErrConflict
}

func TestDraftRequiresAuthentication(t *testing.T) {
	h := NewHandler(conflictTuningRepo{}, nil, nil)
	r := httptest.NewRequest(http.MethodPut, "/api/workspaces/w/tuning/draft", strings.NewReader(`{"expected_revision":0,"parameters":{}}`))
	w := httptest.NewRecorder()
	h.Draft(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", w.Code)
	}
}
func TestDraftStrictJSONRejectsUnknownField(t *testing.T) {
	h := NewHandler(conflictTuningRepo{}, nil, nil)
	r := httptest.NewRequest(http.MethodPut, "/api/workspaces/w/tuning/draft", strings.NewReader(`{"expected_revision":0,"parameters":{},"unknown":true}`))
	r = r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{UserID: "u"}))
	w := httptest.NewRecorder()
	h.Draft(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}
func TestDraftMapsRepositoryConflict(t *testing.T) {
	h := NewHandler(conflictTuningRepo{}, nil, nil)
	r := httptest.NewRequest(http.MethodPut, "/api/workspaces/w/tuning/draft", strings.NewReader(`{"expected_revision":0,"parameters":{}}`))
	r = r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{UserID: "u"}))
	w := httptest.NewRecorder()
	h.Draft(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", w.Code)
	}
}
