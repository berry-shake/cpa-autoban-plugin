package plugin

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// pick filters out credentials that are still banned.
//
// CPA integration rules:
//  1. When no candidate is banned, return Handled: false so the host selector
//     runs untouched, preserving the configured routing strategy and session
//     affinity (prompt cache reuse).
//  2. When every candidate is banned, return Handled: false so CPA can apply
//     its native cooldown / failover / error handling.
//  3. When a subset is banned, DelegateBuiltin cannot restrict the host to
//     the healthy subset (it always works over the full candidate set), so
//     the plugin picks a healthy AuthID itself:
//     - only the highest priority tier is considered;
//     - a request carrying a session marker is hashed onto a stable healthy
//     credential (weighted), so the session keeps one account;
//     - otherwise smooth weighted round-robin spreads the load.
func (s *Service) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	now := s.now()
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		// Only Codex credentials are subject to bans.
		if strings.EqualFold(candidate.Provider, providerCodex) && s.bans.active(candidate.ID, now) {
			continue
		}
		available = append(available, candidate)
	}

	if len(available) == 0 || len(available) == len(req.Candidates) {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	maxPriority := available[0].Priority
	for _, c := range available[1:] {
		if c.Priority > maxPriority {
			maxPriority = c.Priority
		}
	}
	bestTier := make([]pluginapi.SchedulerAuthCandidate, 0, len(available))
	for _, c := range available {
		if c.Priority == maxPriority {
			bestTier = append(bestTier, c)
		}
	}

	var chosen pluginapi.SchedulerAuthCandidate
	sessionID := extractSessionID(req.Options)
	switch {
	case len(bestTier) == 1:
		chosen = bestTier[0]
	case sessionID != "":
		chosen = pickBySessionAffinity(bestTier, sessionID)
	default:
		chosen = s.wrr.pick(bestTier)
	}
	return pluginapi.SchedulerPickResponse{AuthID: chosen.ID, Handled: true}
}

var sessionHeaderKeys = [...]string{
	"session-id",
	"session_id",
	"x-session-id",
	"x-session-affinity",
	"x-slot-session-id",
	"x-conversation-id",
	"x-thread-id",
	"x-client-request-id",
	"x-claude-code-session-id",
}

var sessionMetadataKeys = [...]string{
	"session_id",
	"execution_session_id",
	"derived_session_id",
	"parent_session_id",
	"conversation_id",
}

// extractSessionID looks for explicit or derived session markers in request
// headers and host metadata.
func extractSessionID(options pluginapi.SchedulerOptions) string {
	for _, key := range sessionHeaderKeys {
		for name, values := range options.Headers {
			if strings.EqualFold(name, key) && len(values) > 0 {
				if val := strings.TrimSpace(values[0]); val != "" {
					return val
				}
			}
		}
	}
	for _, key := range sessionMetadataKeys {
		if str, ok := options.Metadata[key].(string); ok {
			if val := strings.TrimSpace(str); val != "" {
				return val
			}
		}
	}
	return ""
}

// candidateWeight extracts the positive integer weight of a candidate,
// defaulting to 1.
func candidateWeight(c pluginapi.SchedulerAuthCandidate) int64 {
	if raw, ok := c.Attributes["weight"]; ok {
		if w, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && w > 0 {
			return w
		}
	}
	switch v := c.Metadata["weight"].(type) {
	case int:
		if v > 0 {
			return int64(v)
		}
	case int64:
		if v > 0 {
			return v
		}
	case float64:
		if v >= 1 {
			return int64(v)
		}
	case string:
		if w, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && w > 0 {
			return w
		}
	}
	return 1
}

// fnv1a64 computes a 64-bit FNV-1a hash of s.
func fnv1a64(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// pickBySessionAffinity deterministically maps sessionID onto one candidate,
// honoring relative weights, so a session keeps reusing one prompt cache.
func pickBySessionAffinity(candidates []pluginapi.SchedulerAuthCandidate, sessionID string) pluginapi.SchedulerAuthCandidate {
	if len(candidates) == 0 {
		return pluginapi.SchedulerAuthCandidate{}
	}
	sorted := append([]pluginapi.SchedulerAuthCandidate(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var totalWeight int64
	for _, c := range sorted {
		totalWeight += candidateWeight(c)
	}
	target := int64(fnv1a64(sessionID) % uint64(totalWeight))
	var cumulative int64
	for _, c := range sorted {
		cumulative += candidateWeight(c)
		if target < cumulative {
			return c
		}
	}
	return sorted[len(sorted)-1]
}

// weightedRoundRobinPicker implements NGINX-style smooth weighted round-robin.
type weightedRoundRobinPicker struct {
	mu            sync.Mutex
	currentWeight map[string]int64
}

func (p *weightedRoundRobinPicker) pick(candidates []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerAuthCandidate {
	if len(candidates) == 0 {
		return pluginapi.SchedulerAuthCandidate{}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.currentWeight == nil {
		p.currentWeight = make(map[string]int64)
	} else if len(p.currentWeight) > 1024 {
		valid := make(map[string]struct{}, len(candidates))
		for _, c := range candidates {
			valid[c.ID] = struct{}{}
		}
		for id := range p.currentWeight {
			if _, ok := valid[id]; !ok {
				delete(p.currentWeight, id)
			}
		}
	}

	var totalWeight int64
	best := candidates[0]
	bestWeight := int64(-1 << 62)
	for _, c := range candidates {
		w := candidateWeight(c)
		totalWeight += w
		p.currentWeight[c.ID] += w
		if p.currentWeight[c.ID] > bestWeight {
			bestWeight = p.currentWeight[c.ID]
			best = c
		}
	}
	p.currentWeight[best.ID] -= totalWeight
	return best
}
