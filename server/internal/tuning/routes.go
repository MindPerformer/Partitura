package tuning

import (
	"net/http"

	"partitura/server/internal/auth"
	"partitura/server/internal/workspace"
)

func RegisterRoutes(mux *http.ServeMux, h *Handler, ws workspace.Repository, ar auth.Repository, cfg auth.AuthConfig) {
	wrap := func(next http.Handler) http.Handler { return auth.AuthMiddleware(ar, cfg)(auth.RequireAuth(next)) }
	admin := func(next http.Handler) http.Handler {
		return auth.AuthMiddleware(ar, cfg)(auth.RequireAuth(workspace.RequireSystemAdmin(next)))
	}
	csrf := func(next http.Handler) http.Handler {
		return auth.AuthMiddleware(ar, cfg)(auth.RequireAuth(auth.RequireCSRF(ar, cfg)(next)))
	}
	wsRead := func(next http.Handler) http.Handler {
		return wrap(workspace.RequireWorkspacePermission(ws, workspace.PermRead, "id")(next))
	}
	wsWrite := func(next http.Handler) http.Handler {
		return csrf(workspace.RequireWorkspacePermission(ws, workspace.PermSettings, "id")(next))
	}
	mux.Handle("GET /api/admin/tuning", admin(http.HandlerFunc(h.state)))
	mux.Handle("PUT /api/admin/tuning/draft", csrf(http.HandlerFunc(h.Draft)))
	mux.Handle("POST /api/admin/tuning/publish", csrf(http.HandlerFunc(h.Publish)))
	mux.Handle("POST /api/admin/tuning/rollback", csrf(http.HandlerFunc(h.Rollback)))
	mux.Handle("DELETE /api/admin/tuning/override", csrf(http.HandlerFunc(h.Clear)))
	mux.Handle("POST /api/admin/tuning/validate", admin(http.HandlerFunc(h.Validate)))
	mux.Handle("GET /api/admin/tuning/recommendations", admin(http.HandlerFunc(h.Recommendations)))
	mux.Handle("GET /api/workspaces/{id}/tuning", wsRead(http.HandlerFunc(h.state)))
	mux.Handle("PUT /api/workspaces/{id}/tuning/draft", wsWrite(http.HandlerFunc(h.Draft)))
	mux.Handle("POST /api/workspaces/{id}/tuning/publish", wsWrite(http.HandlerFunc(h.Publish)))
	mux.Handle("POST /api/workspaces/{id}/tuning/rollback", wsWrite(http.HandlerFunc(h.Rollback)))
	mux.Handle("DELETE /api/workspaces/{id}/tuning/override", wsWrite(http.HandlerFunc(h.Clear)))
	mux.Handle("POST /api/workspaces/{id}/tuning/validate", wsRead(http.HandlerFunc(h.Validate)))
	mux.Handle("GET /api/workspaces/{id}/tuning/recommendations", wsRead(http.HandlerFunc(h.Recommendations)))
}
