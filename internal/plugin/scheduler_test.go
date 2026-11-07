package plugin

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func pick(t *testing.T, s *Service, req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	t.Helper()
	var resp pluginapi.SchedulerPickResponse
	callRPC(t, s, pluginabi.MethodSchedulerPick, req, &resp)
	return resp
}

func banFor(s *Service, authID string, d time.Duration) {
	s.bans.set(authID, banEntry{ResetAt: testNow.Add(d), Window: "5h", BannedAt: testNow})
}

func codexCandidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: id, Provider: providerCodex})
	}
	return out
}

func TestPickAllHealthyLeavesHostRouting(t *testing.T) {
	s := newTestService()
	resp := pick(t, s, pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: codexCandidates("auth-1", "auth-2", "auth-3")})
	if resp.Handled || resp.DelegateBuiltin != "" || resp.AuthID != "" {
		t.Fatalf("all healthy: want an unhandled empty response, got %+v", resp)
	}
}

func TestPickAllBannedLeavesHostRouting(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-1", 2*time.Hour)
	banFor(s, "auth-2", 2*time.Hour)
	resp := pick(t, s, pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: codexCandidates("auth-1", "auth-2")})
	if resp.Handled {
		t.Fatalf("all banned: want Handled=false, got %+v", resp)
	}
}

func TestPickPartialBannedUsesHighestRemainingPriority(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-3", 2*time.Hour)
	resp := pick(t, s, pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-1", Provider: providerCodex, Priority: 10},
			{ID: "auth-2", Provider: providerCodex, Priority: 5},
			{ID: "auth-3", Provider: providerCodex, Priority: 20},
		},
	})
	if !resp.Handled || resp.AuthID != "auth-1" {
		t.Fatalf("want auth-1 (highest remaining tier), got %+v", resp)
	}
}

func TestPickPartialBannedKeepsSessionOnOneCredential(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-1", 2*time.Hour)
	candidates := codexCandidates("auth-1", "auth-2", "auth-3", "auth-4")

	for name, options := range map[string]pluginapi.SchedulerOptions{
		"header":   {Headers: http.Header{"Session-Id": {"sticky-session-abc"}}},
		"metadata": {Metadata: map[string]any{"session_id": "another-session-xyz"}},
	} {
		var first string
		for i := 0; i < 20; i++ {
			resp := pick(t, s, pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: candidates, Options: options})
			if !resp.Handled || resp.AuthID == "" || resp.AuthID == "auth-1" {
				t.Fatalf("%s #%d: want a healthy handled pick, got %+v", name, i, resp)
			}
			if i == 0 {
				first = resp.AuthID
			} else if resp.AuthID != first {
				t.Fatalf("%s #%d: session moved from %s to %s", name, i, first, resp.AuthID)
			}
		}
	}
}

func TestPickPartialBannedRoundRobinsWithoutSession(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-1", 2*time.Hour)
	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		resp := pick(t, s, pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: codexCandidates("auth-1", "auth-2", "auth-3")})
		if !resp.Handled || resp.AuthID == "auth-1" {
			t.Fatalf("#%d: want a healthy handled pick, got %+v", i, resp)
		}
		counts[resp.AuthID]++
	}
	if counts["auth-2"] != 5 || counts["auth-3"] != 5 {
		t.Fatalf("equal weights: want 5/5, got %v", counts)
	}
}

func TestPickPartialBannedHonorsWeights(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-1", 2*time.Hour)
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-1", Provider: providerCodex},
		{ID: "auth-2", Provider: providerCodex, Attributes: map[string]string{"weight": "3"}},
		{ID: "auth-3", Provider: providerCodex, Attributes: map[string]string{"weight": "1"}},
	}
	counts := map[string]int{}
	for i := 0; i < 12; i++ {
		counts[pick(t, s, pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: candidates}).AuthID]++
	}
	if counts["auth-2"] != 9 || counts["auth-3"] != 3 {
		t.Fatalf("3:1 weights over 12 picks: want 9/3, got %v", counts)
	}
}

func TestPickReEnablesExpiredBan(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-1", -10*time.Minute)
	resp := pick(t, s, pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: codexCandidates("auth-1", "auth-2")})
	if resp.Handled {
		t.Fatalf("expired ban: want Handled=false, got %+v", resp)
	}
	if len(s.bans.snapshot(testNow)) != 0 {
		t.Fatal("expired ban was not cleared")
	}
}

func TestPickIgnoresNonCodexCandidates(t *testing.T) {
	s := newTestService()
	banFor(s, "claude-1", 2*time.Hour)
	resp := pick(t, s, pluginapi.SchedulerPickRequest{
		Provider: "claude",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "claude-1", Provider: "claude"},
			{ID: "claude-2", Provider: "claude"},
		},
	})
	if resp.Handled {
		t.Fatalf("non-codex candidates: want Handled=false, got %+v", resp)
	}
}

func TestPickSessionAffinitySurvivesFractionalWeights(t *testing.T) {
	s := newTestService()
	banFor(s, "auth-1", 2*time.Hour)
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-1", Provider: providerCodex},
		{ID: "auth-2", Provider: providerCodex, Metadata: map[string]any{"weight": 0.5}},
		{ID: "auth-3", Provider: providerCodex, Metadata: map[string]any{"weight": 0.5}},
	}
	resp := pick(t, s, pluginapi.SchedulerPickRequest{
		Provider:   providerCodex,
		Candidates: candidates,
		Options:    pluginapi.SchedulerOptions{Headers: http.Header{"X-Session-Id": {"s"}}},
	})
	if !resp.Handled || (resp.AuthID != "auth-2" && resp.AuthID != "auth-3") {
		t.Fatalf("want a healthy handled pick, got %+v", resp)
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name    string
		options pluginapi.SchedulerOptions
		want    string
	}{
		{name: "empty", options: pluginapi.SchedulerOptions{}, want: ""},
		{name: "header session-id", options: pluginapi.SchedulerOptions{Headers: http.Header{"Session-Id": {"test-session-1"}}}, want: "test-session-1"},
		{name: "header case insensitive", options: pluginapi.SchedulerOptions{Headers: http.Header{"X-SESSION-AFFINITY": {"test-affinity-2"}}}, want: "test-affinity-2"},
		{name: "metadata session_id", options: pluginapi.SchedulerOptions{Metadata: map[string]any{"session_id": "test-meta-3"}}, want: "test-meta-3"},
		{name: "metadata execution_session_id", options: pluginapi.SchedulerOptions{Metadata: map[string]any{"execution_session_id": "test-exec-4"}}, want: "test-exec-4"},
		{name: "blank values skipped", options: pluginapi.SchedulerOptions{Headers: http.Header{"Session-Id": {"  "}}, Metadata: map[string]any{"conversation_id": "c-5"}}, want: "c-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractSessionID(tt.options); got != tt.want {
				t.Fatalf("extractSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}
