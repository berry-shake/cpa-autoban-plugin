package plugin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type banStatus struct {
	Plugin  string    `json:"plugin"`
	Version string    `json:"version"`
	Now     string    `json:"now"`
	Count   int       `json:"count"`
	Bans    []banInfo `json:"bans"`
}

type banInfo struct {
	AuthID           string `json:"auth_id"`
	Window           string `json:"window"`
	BannedAt         string `json:"banned_at,omitempty"`
	ResetAt          string `json:"reset_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
}

type unbanRequest struct {
	AuthID string `json:"auth_id"`
	All    bool   `json:"all"`
}

type unbanResponse struct {
	OK      bool      `json:"ok"`
	AuthID  string    `json:"auth_id,omitempty"`
	Removed int       `json:"removed"`
	Status  banStatus `json:"status"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// handleManagement serves both route families. The status page and logo are
// unauthenticated GET resources; bans and unban are /v0/management routes,
// and CPA checks the management key before forwarding them.
func (s *Service) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	switch path {
	case logoPath:
		if method == http.MethodGet {
			return logoResponse()
		}
	case pagePath:
		if method == http.MethodGet {
			return pageResponse()
		}
	case bansPath:
		if method == http.MethodGet {
			return jsonResponse(http.StatusOK, s.status())
		}
	case unbanPath:
		// POST only: unbanning changes routing, so a plain link or prefetch
		// must never trigger it.
		if method == http.MethodPost {
			return s.unban(req)
		}
	case unbanAllPath:
		if method == http.MethodPost {
			return s.unbanAll()
		}
	default:
		return jsonResponse(http.StatusNotFound, errorBody{Error: "not_found", Message: "plugin route not found"})
	}
	return jsonResponse(http.StatusMethodNotAllowed, errorBody{Error: "method_not_allowed", Message: "method not allowed on this route"})
}

func (s *Service) status() banStatus {
	now := s.now()
	snapshot := s.bans.snapshot(now)
	bans := make([]banInfo, 0, len(snapshot))
	for authID, entry := range snapshot {
		info := banInfo{
			AuthID:           authID,
			Window:           entry.Window,
			ResetAt:          entry.ResetAt.UTC().Format(time.RFC3339),
			RemainingSeconds: int64(entry.ResetAt.Sub(now).Seconds()),
		}
		if !entry.BannedAt.IsZero() {
			info.BannedAt = entry.BannedAt.UTC().Format(time.RFC3339)
		}
		bans = append(bans, info)
	}
	sort.Slice(bans, func(i, j int) bool {
		if bans[i].ResetAt == bans[j].ResetAt {
			return bans[i].AuthID < bans[j].AuthID
		}
		return bans[i].ResetAt < bans[j].ResetAt
	})
	return banStatus{
		Plugin:  PluginID,
		Version: s.version,
		Now:     now.UTC().Format(time.RFC3339),
		Count:   len(bans),
		Bans:    bans,
	}
}

func (s *Service) unban(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body unbanRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, errorBody{Error: "invalid_json", Message: errUnmarshal.Error()})
		}
	}
	if body.All || strings.EqualFold(req.Query.Get("all"), "true") {
		return s.unbanAll()
	}
	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(req.Query.Get("auth_id"))
	}
	if authID == "" {
		return jsonResponse(http.StatusBadRequest, errorBody{Error: "missing_auth_id", Message: "provide auth_id in the JSON body or query string"})
	}
	removed := 0
	if entry, ok := s.bans.clear(authID); ok {
		removed = 1
		slog.Info("codex-429-autoban: manually re-enabled credential",
			"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
	}
	return jsonResponse(http.StatusOK, unbanResponse{OK: true, AuthID: authID, Removed: removed, Status: s.status()})
}

func (s *Service) unbanAll() pluginapi.ManagementResponse {
	removed := s.bans.clearAll()
	if removed > 0 {
		slog.Info("codex-429-autoban: manually re-enabled all credentials", "removed", removed)
	}
	return jsonResponse(http.StatusOK, unbanResponse{OK: true, Removed: removed, Status: s.status()})
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(errorBody{Error: "marshal_failed", Message: errMarshal.Error()})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: raw,
	}
}
