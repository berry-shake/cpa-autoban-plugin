package plugin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// providerCodex is the CPA provider key for OpenAI Codex (ChatGPT backend).
	providerCodex = "codex"

	// Codex rate-limit window sizes, in minutes, as reported by the
	// x-codex-primary-window-minutes / x-codex-secondary-window-minutes
	// response headers.
	windowMinutes5h   = 300   // 5 hours
	windowMinutesWeek = 10080 // 7 days

	// usedPercentThreshold marks the window that tripped: a 429 carries the
	// exhausted window at ~100% used.
	usedPercentThreshold = 100

	// fallbackBan is used when a 429 carries no usable x-codex-* headers, so
	// the credential is not hammered while rate-limited.
	fallbackBan = 5 * time.Hour
)

// banEntry records when a held-out credential may be used again.
type banEntry struct {
	// ResetAt is the upstream-reported time at which the exhausted window
	// refreshes. The credential is skipped until now >= ResetAt.
	ResetAt time.Time
	// Window labels which limit was hit ("5h", "week", ...).
	Window string
	// BannedAt is when the ban was recorded.
	BannedAt time.Time
}

// BanStore holds, per credential ID, the time at which it may be used again.
// A credential is absent from the map when it is not banned. State is
// in-memory only and does not survive a CPA restart.
type BanStore struct {
	mu   sync.Mutex
	bans map[string]banEntry
}

// NewBanStore returns an empty ban store.
func NewBanStore() *BanStore {
	return &BanStore{bans: make(map[string]banEntry)}
}

func (s *BanStore) set(authID string, entry banEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bans[authID] = entry
}

// active reports whether authID is still banned at now, clearing the entry
// when its reset time has passed.
func (s *BanStore) active(authID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[authID]
	if !ok {
		return false
	}
	if now.Before(entry.ResetAt) {
		return true
	}
	delete(s.bans, authID)
	slog.Info("autoban: auto re-enabled credential",
		"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
	return false
}

// snapshot drops expired bans and returns a copy of the remaining ones.
func (s *BanStore) snapshot(now time.Time) map[string]banEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]banEntry, len(s.bans))
	for authID, entry := range s.bans {
		if !now.Before(entry.ResetAt) {
			delete(s.bans, authID)
			slog.Info("autoban: auto re-enabled credential",
				"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
			continue
		}
		out[authID] = entry
	}
	return out
}

func (s *BanStore) clear(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[authID]
	if ok {
		delete(s.bans, authID)
	}
	return entry, ok
}

func (s *BanStore) clearAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.bans)
	s.bans = make(map[string]banEntry)
	return n
}

// handleUsage records a ban when a Codex credential receives a 429 and is a
// no-op for everything else. Usage delivery is fire-and-forget, so decode
// failures are only logged.
func (s *Service) handleUsage(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		slog.Warn("autoban: failed to decode usage record", "error", errUnmarshal)
		return
	}
	if !strings.EqualFold(record.Provider, providerCodex) {
		return
	}
	if !record.Failed || record.Failure.StatusCode != http.StatusTooManyRequests {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		slog.Warn("autoban: 429 received but AuthID is empty, cannot ban")
		return
	}

	now := s.now()
	entry, ok := classifyAndBuildBan(record.ResponseHeaders)
	if !ok {
		entry = banEntry{ResetAt: now.Add(fallbackBan), Window: "5h (fallback, headers missing)"}
		slog.Warn("autoban: x-codex-* headers missing on 429, falling back to 5h ban", "auth_id", authID)
	}
	entry.BannedAt = now
	s.bans.set(authID, entry)
	slog.Info("autoban: banned credential after 429",
		"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
}

// classifyAndBuildBan inspects the upstream x-codex-* response headers and
// decides which rate-limit window was exhausted, returning the ban entry with
// the corresponding reset time. It returns ok=false when the headers are
// absent or inconclusive.
//
// Header reference (ChatGPT/Codex backend, not the public Platform API):
//   - x-codex-primary-window-minutes   = 300 for the 5-hour window
//   - x-codex-primary-reset-at         = Unix seconds, 5-hour window reset
//   - x-codex-primary-used-percent     = 0-100
//   - x-codex-secondary-window-minutes = 10080 for the weekly window
//   - x-codex-secondary-reset-at       = Unix seconds, weekly window reset
//   - x-codex-secondary-used-percent   = 0-100
func classifyAndBuildBan(h http.Header) (banEntry, bool) {
	primaryUsed := headerFloat(h, "x-codex-primary-used-percent")
	secondaryUsed := headerFloat(h, "x-codex-secondary-used-percent")
	primaryReset := headerUnixTime(h, "x-codex-primary-reset-at")
	secondaryReset := headerUnixTime(h, "x-codex-secondary-reset-at")

	primaryFull := primaryUsed >= usedPercentThreshold
	secondaryFull := secondaryUsed >= usedPercentThreshold

	switch {
	case secondaryFull && !primaryFull:
		if !secondaryReset.IsZero() {
			return banEntry{ResetAt: secondaryReset, Window: "week"}, true
		}
	case primaryFull && !secondaryFull:
		if !primaryReset.IsZero() {
			return banEntry{ResetAt: primaryReset, Window: "5h"}, true
		}
	case primaryFull && secondaryFull:
		// Both exhausted: wait for the later (weekly) reset.
		if !secondaryReset.IsZero() {
			return banEntry{ResetAt: secondaryReset, Window: "week (both full)"}, true
		}
		if !primaryReset.IsZero() {
			return banEntry{ResetAt: primaryReset, Window: "5h (both full, weekly reset missing)"}, true
		}
	default:
		// Neither reports full via used-percent. Fall back to the window
		// identity when a reset time is present, else give up.
		if !primaryReset.IsZero() && headerInt(h, "x-codex-primary-window-minutes") == windowMinutes5h {
			return banEntry{ResetAt: primaryReset, Window: "5h"}, true
		}
		if !secondaryReset.IsZero() && headerInt(h, "x-codex-secondary-window-minutes") == windowMinutesWeek {
			return banEntry{ResetAt: secondaryReset, Window: "week"}, true
		}
	}
	return banEntry{}, false
}

func headerFloat(h http.Header, key string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(key)), 64)
	if err != nil {
		return 0
	}
	return v
}

func headerInt(h http.Header, key string) int {
	v, err := strconv.Atoi(strings.TrimSpace(h.Get(key)))
	if err != nil {
		return 0
	}
	return v
}

func headerUnixTime(h http.Header, key string) time.Time {
	secs, err := strconv.ParseInt(strings.TrimSpace(h.Get(key)), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}
