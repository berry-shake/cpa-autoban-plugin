package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestClassifyAndBuildBan(t *testing.T) {
	primaryReset := testNow.Add(3 * time.Hour).Truncate(time.Second)
	secondaryReset := testNow.Add(4 * 24 * time.Hour).Truncate(time.Second)
	tests := []struct {
		name       string
		headers    http.Header
		wantOK     bool
		wantReset  time.Time
		wantWindow string
	}{
		{
			name: "5h window full",
			headers: http.Header{
				"X-Codex-Primary-Used-Percent":   {"100"},
				"X-Codex-Primary-Reset-At":       {unix(primaryReset)},
				"X-Codex-Secondary-Used-Percent": {"40"},
				"X-Codex-Secondary-Reset-At":     {unix(secondaryReset)},
			},
			wantOK: true, wantReset: primaryReset, wantWindow: "5h",
		},
		{
			name: "weekly window full",
			headers: http.Header{
				"X-Codex-Primary-Used-Percent":   {"20"},
				"X-Codex-Primary-Reset-At":       {unix(primaryReset)},
				"X-Codex-Secondary-Used-Percent": {"100.0"},
				"X-Codex-Secondary-Reset-At":     {unix(secondaryReset)},
			},
			wantOK: true, wantReset: secondaryReset, wantWindow: "week",
		},
		{
			name: "both full waits for weekly",
			headers: http.Header{
				"X-Codex-Primary-Used-Percent":   {"100"},
				"X-Codex-Primary-Reset-At":       {unix(primaryReset)},
				"X-Codex-Secondary-Used-Percent": {"100"},
				"X-Codex-Secondary-Reset-At":     {unix(secondaryReset)},
			},
			wantOK: true, wantReset: secondaryReset, wantWindow: "week (both full)",
		},
		{
			name: "window identity fallback",
			headers: http.Header{
				"X-Codex-Primary-Window-Minutes": {"300"},
				"X-Codex-Primary-Reset-At":       {unix(primaryReset)},
			},
			wantOK: true, wantReset: primaryReset, wantWindow: "5h",
		},
		{name: "no headers", headers: nil, wantOK: false},
		{
			name:    "full but no reset time",
			headers: http.Header{"X-Codex-Primary-Used-Percent": {"100"}},
			wantOK:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := classifyAndBuildBan(tt.headers)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !entry.ResetAt.Equal(tt.wantReset) || entry.Window != tt.wantWindow {
				t.Fatalf("entry = %+v, want reset %v window %q", entry, tt.wantReset, tt.wantWindow)
			}
		})
	}
}

func TestUsageBansCodex429(t *testing.T) {
	s := newTestService()
	reset := testNow.Add(90 * time.Minute).Truncate(time.Second)
	callRPC(t, s, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "codex-a.json",
		Failed:   true,
		Failure:  pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent": {"100"},
			"X-Codex-Primary-Reset-At":     {unix(reset)},
		},
	}, nil)
	bans := s.bans.snapshot(testNow)
	entry, ok := bans["codex-a.json"]
	if !ok || !entry.ResetAt.Equal(reset) || entry.Window != "5h" || !entry.BannedAt.Equal(testNow) {
		t.Fatalf("bans = %+v", bans)
	}
}

func TestUsageFallsBackToFiveHoursWithoutHeaders(t *testing.T) {
	s := newTestService()
	callRPC(t, s, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		Provider: "Codex",
		AuthID:   "codex-b.json",
		Failed:   true,
		Failure:  pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
	}, nil)
	entry, ok := s.bans.snapshot(testNow)["codex-b.json"]
	if !ok || !entry.ResetAt.Equal(testNow.Add(fallbackBan)) {
		t.Fatalf("fallback ban = %+v (ok=%v)", entry, ok)
	}
}

func TestUsageIgnoresOtherRecords(t *testing.T) {
	s := newTestService()
	for name, record := range map[string]pluginapi.UsageRecord{
		"other provider": {Provider: "claude", AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}},
		"not failed":     {Provider: "codex", AuthID: "a"},
		"other status":   {Provider: "codex", AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 500}},
		"missing auth":   {Provider: "codex", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}},
	} {
		callRPC(t, s, pluginabi.MethodUsageHandle, record, nil)
		if n := len(s.bans.snapshot(testNow)); n != 0 {
			t.Fatalf("%s: recorded %d bans", name, n)
		}
	}
}

func TestUsageToleratesBadPayload(t *testing.T) {
	s := newTestService()
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(s.Call(pluginabi.MethodUsageHandle, []byte("{")), &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("bad usage payload must still ack: env=%+v err=%v", env, errUnmarshal)
	}
}
