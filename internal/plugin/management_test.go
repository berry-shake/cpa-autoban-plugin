package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func manage(t *testing.T, s *Service, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	var resp pluginapi.ManagementResponse
	callRPC(t, s, pluginabi.MethodManagementHandle, req, &resp)
	return resp
}

func decodeBody(t *testing.T, resp pluginapi.ManagementResponse, out any) {
	t.Helper()
	if errUnmarshal := json.Unmarshal(resp.Body, out); errUnmarshal != nil {
		t.Fatalf("decode body %q: %v", resp.Body, errUnmarshal)
	}
}

func TestBansListsActiveBansSortedByReset(t *testing.T) {
	s := newTestService()
	banFor(s, "late", 3*time.Hour)
	banFor(s, "early", time.Hour)
	banFor(s, "expired", -time.Minute)

	resp := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodGet, Path: bansPath})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var status banStatus
	decodeBody(t, resp, &status)
	if status.Count != 2 || status.Bans[0].AuthID != "early" || status.Bans[1].AuthID != "late" {
		t.Fatalf("status = %+v", status)
	}
	if status.Bans[0].RemainingSeconds != 3600 || status.Version != "1.2.3-test" {
		t.Fatalf("status = %+v", status)
	}
}

func TestUnbanOneByBodyAndQuery(t *testing.T) {
	s := newTestService()
	banFor(s, "a", time.Hour)
	banFor(s, "b", time.Hour)

	var out unbanResponse
	decodeBody(t, manage(t, s, pluginapi.ManagementRequest{Method: http.MethodPost, Path: unbanPath, Body: []byte(`{"auth_id":"a"}`)}), &out)
	if !out.OK || out.Removed != 1 || out.Status.Count != 1 {
		t.Fatalf("unban by body = %+v", out)
	}
	decodeBody(t, manage(t, s, pluginapi.ManagementRequest{Method: http.MethodPost, Path: unbanPath, Query: url.Values{"auth_id": {"b"}}}), &out)
	if out.Removed != 1 || out.Status.Count != 0 {
		t.Fatalf("unban by query = %+v", out)
	}
	decodeBody(t, manage(t, s, pluginapi.ManagementRequest{Method: http.MethodPost, Path: unbanPath, Body: []byte(`{"auth_id":"b"}`)}), &out)
	if !out.OK || out.Removed != 0 {
		t.Fatalf("unban of a non-banned auth = %+v", out)
	}
}

func TestUnbanRejectsBadInput(t *testing.T) {
	s := newTestService()
	if resp := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodPost, Path: unbanPath}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing auth_id: status = %d", resp.StatusCode)
	}
	if resp := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodPost, Path: unbanPath, Body: []byte("{")}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: status = %d", resp.StatusCode)
	}
}

func TestUnbanAll(t *testing.T) {
	for name, req := range map[string]pluginapi.ManagementRequest{
		"unban-all":      {Method: http.MethodPost, Path: unbanAllPath},
		"unban all=true": {Method: http.MethodPost, Path: unbanPath, Query: url.Values{"all": {"true"}}},
		"unban body all": {Method: http.MethodPost, Path: unbanPath, Body: []byte(`{"all":true}`)},
		"trailing slash": {Method: http.MethodPost, Path: unbanAllPath + "/"},
		"lowercase verb": {Method: "post", Path: unbanAllPath},
	} {
		s := newTestService()
		banFor(s, "a", time.Hour)
		banFor(s, "b", time.Hour)
		var out unbanResponse
		decodeBody(t, manage(t, s, req), &out)
		if out.Removed != 2 || out.Status.Count != 0 {
			t.Fatalf("%s: %+v", name, out)
		}
	}
}

func TestManagementRejectsWrongMethodAndPath(t *testing.T) {
	s := newTestService()
	banFor(s, "a", time.Hour)
	if resp := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodGet, Path: unbanAllPath}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET unban-all: status = %d", resp.StatusCode)
	}
	if len(s.bans.snapshot(testNow)) != 1 {
		t.Fatal("GET unban-all must not change state")
	}
	if resp := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBase + "/nope"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: status = %d", resp.StatusCode)
	}
}

func TestStatusPageAndLogo(t *testing.T) {
	s := newTestService()
	page := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodGet, Path: pagePath})
	if page.StatusCode != http.StatusOK || !strings.HasPrefix(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("page response = %d %v", page.StatusCode, page.Headers)
	}
	if !strings.Contains(page.Headers.Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Fatalf("page CSP = %q", page.Headers.Get("Content-Security-Policy"))
	}
	if !bytes.Contains(page.Body, []byte("cli-proxy-auth")) || bytes.Contains(page.Body, []byte("localStorage.setItem('codex429AutobanManagementKey'")) {
		t.Fatal("page must reuse the panel key and never persist its own copy")
	}
	logo := manage(t, s, pluginapi.ManagementRequest{Method: http.MethodGet, Path: logoPath})
	if logo.StatusCode != http.StatusOK || logo.Headers.Get("Content-Type") != "image/png" || !bytes.HasPrefix(logo.Body, []byte("\x89PNG")) {
		t.Fatalf("logo response = %d %v len=%d", logo.StatusCode, logo.Headers, len(logo.Body))
	}
}
