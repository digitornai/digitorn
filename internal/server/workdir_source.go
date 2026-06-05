package server

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
	"github.com/mbathepaul/digitorn/internal/runtime/workdir"
)

// sessionPathPolicies implements runtime.PathPolicySource over the session
// store (the per-session workdir resolved at session creation) + the app's
// merged module constraints. The engine calls PathPolicyFor once per turn and
// attaches the policy to the dispatch ctx ; the single chokepoint then confines
// every path-typed tool arg to the session's workdir.
type sessionPathPolicies struct {
	store *sessionstore.Bus
	apps  appmgr.Manager
}

// PathPolicyFor returns the session's workdir PathPolicy. ok=false when the
// session has no workdir (mode none / chat with nothing supplied) → the
// chokepoint skips path confinement and the module's static fallback applies.
func (s sessionPathPolicies) PathPolicyFor(appID, sessionID string) (workdir.PathPolicy, bool) {
	if s.store == nil {
		return workdir.PathPolicy{}, false
	}
	// A delegated sub-agent runs in an ISOLATED sub-session (root::agent::<runID>)
	// — its conversation never sees the parent's — but it SHARES the root
	// session's workdir, so the coordinator and its sub-agents read/write the
	// same files. The sub-session itself carries no workdir (it is created
	// implicitly, not via createSession), so resolve the policy from the ROOT.
	lookupID := sessionID
	if root, _, isSub := sessionstore.SubAgentSession(sessionID); isSub {
		lookupID = root
	}
	st, err := s.store.State(lookupID)
	if err != nil || st == nil {
		return workdir.PathPolicy{}, false
	}
	st.RLock()
	root := st.Workdir
	st.RUnlock()
	if root == "" {
		return workdir.PathPolicy{}, false
	}
	extra, unrestricted := s.appConstraints(appID)
	return workdir.NewPolicy(workdir.Options{
		Root:         root,
		AllowedExtra: extra,
		Unrestricted: unrestricted,
	}), true
}

// appConstraints merges allowed_paths + unrestricted across every module's
// tools.modules.<id>.constraints — one policy for the whole app, matching the
// workdir-sandbox doc ("constraints are merged across every agent-facing
// module into one policy").
func (s sessionPathPolicies) appConstraints(appID string) (allowed []string, unrestricted bool) {
	if s.apps == nil {
		return nil, false
	}
	rt, err := s.apps.Get(context.Background(), appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Tools == nil {
		return nil, false
	}
	for _, mb := range rt.Definition.Tools.Modules {
		if mb.Constraints == nil {
			continue
		}
		if v, ok := mb.Constraints["unrestricted"].(bool); ok && v {
			unrestricted = true
		}
		switch ap := mb.Constraints["allowed_paths"].(type) {
		case []any:
			for _, p := range ap {
				if str, ok := p.(string); ok && str != "" {
					allowed = append(allowed, str)
				}
			}
		case []string:
			allowed = append(allowed, ap...)
		}
	}
	return allowed, unrestricted
}
