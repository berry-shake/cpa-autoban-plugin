package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestRegisterNegotiatesSchemaVersion(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    uint32
	}{
		{name: "no payload", payload: nil, want: 1},
		{name: "old host", payload: map[string]any{"schema_version": 3}, want: 3},
		{name: "newer host", payload: map[string]any{"schema_version": 10}, want: maxSchemaVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reg rpcRegistration
			callRPC(t, newTestService(), pluginabi.MethodPluginRegister, tt.payload, &reg)
			if reg.SchemaVersion != tt.want {
				t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, tt.want)
			}
		})
	}
}

func TestRegisterDeclaresCapabilitiesAndLogo(t *testing.T) {
	var reg rpcRegistration
	callRPC(t, newTestService(), pluginabi.MethodPluginRegister, map[string]any{"schema_version": 10}, &reg)
	if !reg.Capabilities.UsagePlugin || !reg.Capabilities.Scheduler || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
	if reg.Metadata.Logo != "/v0/resource/plugins/codex-429-autoban/logo.png" {
		t.Fatalf("logo = %q", reg.Metadata.Logo)
	}
	if reg.Metadata.Version != "1.2.3-test" || reg.Metadata.GitHubRepository != repository {
		t.Fatalf("metadata = %+v", reg.Metadata)
	}
}

func TestManagementRegisterDeclaresRoutes(t *testing.T) {
	var reg managementRegistration
	callRPC(t, newTestService(), pluginabi.MethodManagementRegister, nil, &reg)
	routes := map[string]string{}
	for _, route := range reg.Routes {
		routes[route.Path] = route.Method
	}
	if routes[bansPath] != "GET" || routes[unbanPath] != "POST" || routes[unbanAllPath] != "POST" {
		t.Fatalf("routes = %+v", reg.Routes)
	}
	resources := map[string]string{}
	for _, resource := range reg.Resources {
		resources[resource.Path] = resource.Menu
	}
	if resources["/status"] != displayName {
		t.Fatalf("status resource missing menu: %+v", reg.Resources)
	}
	if _, ok := resources["/logo.png"]; !ok {
		t.Fatalf("logo resource missing: %+v", reg.Resources)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(newTestService().Call("nope", nil), &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestBanStoreSharedAcrossServices(t *testing.T) {
	store := NewBanStore()
	first := New("v", store)
	first.bans.set("auth-1", banEntry{ResetAt: testNow.Add(time.Hour)})
	second := New("v", store)
	if !second.bans.active("auth-1", testNow) {
		t.Fatal("a new service built on the same store lost the ban")
	}
}
