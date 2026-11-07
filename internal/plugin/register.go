// Package plugin implements the autoban plugin runtime.
//
// Three capabilities are registered:
//   - usage_plugin: on a Codex 429 it reads the upstream x-codex-* response
//     headers, decides whether the 5-hour window or the weekly cap was
//     exhausted, and records when the credential may be used again.
//   - scheduler: drops candidates whose recorded reset time has not passed
//     (lazy re-enable, since CPA exposes no timer hook). When nothing is
//     banned it returns Handled: false so the host routing (strategy,
//     session affinity) stays in charge.
//   - management_api: a status page plus authenticated routes to list bans
//     and put credentials back after a manual quota reset upstream.
package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// PluginID is the store and runtime identifier. It deliberately differs
	// from the upstream ysxk/codex-429-autoban so both can be listed in plugin
	// stores without colliding.
	PluginID = "autoban"

	displayName = "Autoban"
	repository  = "https://github.com/berry-shake/cpa-autoban-plugin"

	managementBase = "/v0/management/plugins/" + PluginID
	resourceBase   = "/v0/resource/plugins/" + PluginID

	bansPath     = managementBase + "/bans"
	unbanPath    = managementBase + "/unban"
	unbanAllPath = managementBase + "/unban-all"
	pagePath     = resourceBase + "/status"
	logoPath     = resourceBase + "/logo.png"
)

// maxSchemaVersion is the newest plugin RPC contract this plugin implements.
// Schema 6 makes the host return management JSON verbatim instead of
// HTML-escaping its strings. It is pinned rather than taken from
// pluginabi.SchemaVersion so an SDK bump cannot silently opt into newer
// contracts or make older hosts refuse to load the plugin.
const maxSchemaVersion = pluginabi.SchemaVersionRawManagementResponse

// Service dispatches plugin lifecycle, usage, scheduler and management RPC methods.
type Service struct {
	version string
	bans    *BanStore
	wrr     *weightedRoundRobinPicker
	now     func() time.Time
}

type rpcCapabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

type rpcRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  rpcCapabilities    `json:"capabilities"`
}

// routeDeclaration is a /v0/management route; CPA checks the management key
// before forwarding the request to the plugin.
type routeDeclaration struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// resourceDeclaration is a /v0/resource route; CPA does not authenticate it.
type resourceDeclaration struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistration struct {
	Routes    []routeDeclaration    `json:"routes,omitempty"`
	Resources []resourceDeclaration `json:"resources,omitempty"`
}

type rpcLifecycleRequest struct {
	SchemaVersion uint32 `json:"schema_version"`
}

// New builds a service around bans. Passing the same store to successive
// services keeps ban state across a plugin shutdown/init cycle.
func New(version string, bans *BanStore) *Service {
	if strings.TrimSpace(version) == "" {
		version = "0.0.0-dev"
	}
	if bans == nil {
		bans = NewBanStore()
	}
	return &Service{
		version: version,
		bans:    bans,
		wrr:     &weightedRoundRobinPicker{},
		now:     time.Now,
	}
}

// Call handles one plugin RPC method and always returns a JSON envelope.
func (s *Service) Call(method string, request []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(s.registration(request))
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodUsageHandle:
		s.handleUsage(request)
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return ErrorEnvelope("invalid_request", "decode scheduler request: "+errUnmarshal.Error())
		}
		return okEnvelope(s.pick(req))
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRoutes())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return ErrorEnvelope("invalid_request", "decode management request: "+errUnmarshal.Error())
		}
		return okEnvelope(s.handleManagement(req))
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func (s *Service) registration(request []byte) rpcRegistration {
	return rpcRegistration{
		SchemaVersion: negotiateSchemaVersion(request),
		Metadata: pluginapi.Metadata{
			Name:             displayName,
			Version:          s.version,
			Author:           "ysxk / berry-shake",
			GitHubRepository: repository,
			Logo:             logoPath,
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: rpcCapabilities{
			UsagePlugin:   true,
			Scheduler:     true,
			ManagementAPI: true,
		},
	}
}

func managementRoutes() managementRegistration {
	return managementRegistration{
		Routes: []routeDeclaration{
			{Method: http.MethodGet, Path: bansPath, Description: "List Codex auths currently held out of the pool by autoban."},
			{Method: http.MethodPost, Path: unbanPath, Description: `Put one Codex auth back into the pool. Body: {"auth_id":"..."}.`},
			{Method: http.MethodPost, Path: unbanAllPath, Description: "Put every Codex auth back into the pool."},
		},
		Resources: []resourceDeclaration{
			{Path: "/status", Menu: displayName, Description: "View and manually unban Codex credentials after a quota reset."},
			{Path: "/logo.png", Description: "Plugin logo served to the management panel sidebar."},
		},
	}
}

// negotiateSchemaVersion answers plugin.register with the newest schema both
// sides implement. Hosts advertise theirs in the lifecycle request; one that
// advertises nothing only knows the original contract.
func negotiateSchemaVersion(request []byte) uint32 {
	var lifecycle rpcLifecycleRequest
	if errUnmarshal := json.Unmarshal(request, &lifecycle); errUnmarshal != nil || lifecycle.SchemaVersion == 0 {
		return 1
	}
	return min(lifecycle.SchemaVersion, maxSchemaVersion)
}

func okEnvelope(result any) []byte {
	rawResult, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return ErrorEnvelope("marshal_failed", errMarshal.Error())
	}
	raw, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: rawResult})
	return raw
}

// ErrorEnvelope builds a failure envelope for RPC and ABI initialization errors.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:    code,
			Message: message,
		},
	})
	return raw
}
