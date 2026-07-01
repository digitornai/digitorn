package validate

import (
	"fmt"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/compiler/suggest"
)

func (v *validator) checkEnums() {
	rt := v.def.Runtime
	if rt != nil {
		v.enum("runtime.mode", string(rt.Mode), modeStrings())
		v.enum("runtime.session_mode", string(rt.SessionMode), enumStrings(schema.AllSessionModes))
		v.enum("runtime.workdir_mode", string(rt.WorkdirMode), enumStrings(schema.AllWorkdirModes))
		v.enum("runtime.tool_injection", string(rt.ToolInjection), enumStrings(schema.AllToolInjections))
		if rt.Input != nil {
			v.enum("runtime.input.type", string(rt.Input.Type), enumStrings(schema.AllInputTypes))
		}
		if rt.Output != nil {
			v.enum("runtime.output.type", string(rt.Output.Type), enumStrings(schema.AllOutputTypes))
		}
		if rt.Context != nil {
			v.enum("runtime.context.strategy", string(rt.Context.Strategy), enumStrings(schema.AllContextStrategies))
		}
		for i, t := range rt.Triggers {
			if t.Method != "" {
				v.enum(fmt.Sprintf("runtime.triggers.%d.method", i), string(t.Method), enumStrings(schema.AllHTTPMethods))
			}
			if t.Routing != "" {
				v.enum(fmt.Sprintf("runtime.triggers.%d.routing", i), string(t.Routing), enumStrings(schema.AllRoutings))
			}
		}
	}

	if v.def.App.Category != "" {
		v.enum("app.category", string(v.def.App.Category), enumStrings(schema.AllCategories))
	}
	if v.def.App.AttachmentsMode != "" {
		v.enum("app.attachments_mode", string(v.def.App.AttachmentsMode), enumStrings(schema.AllAttachmentsModes))
	}

	for i, a := range v.def.Agents {
		if a.Role != "" {
			v.enum(fmt.Sprintf("agents.%d.role", i),
				a.Role, enumStrings(schema.KnownRoles))
		}
		if a.Brain.Backend != "" {
			v.enum(fmt.Sprintf("agents.%d.brain.backend", i),
				string(a.Brain.Backend), enumStrings(schema.AllBackends))
		}
		if a.Brain.ImageDetail != "" {
			v.enum(fmt.Sprintf("agents.%d.brain.image_detail", i),
				string(a.Brain.ImageDetail), enumStrings(schema.AllImageDetails))
		}
	}

	if t := v.def.Tools; t != nil && t.Capabilities != nil {
		if t.Capabilities.DefaultPolicy != "" {
			v.enum("tools.capabilities.default_policy",
				string(t.Capabilities.DefaultPolicy), enumStrings(schema.AllCapabilityPolicies))
		}
		if t.Capabilities.MaxRiskLevel != "" {
			v.enum("tools.capabilities.max_risk_level",
				string(t.Capabilities.MaxRiskLevel), enumStrings(schema.AllRiskLevels))
		}
	}

	if s := v.def.Security; s != nil {
		if s.Sandbox != nil && s.Sandbox.Level != "" {
			v.enum("security.sandbox.level", string(s.Sandbox.Level), enumStrings(schema.AllSandboxLevels))
		}
		if s.CredentialsSchema != nil {
			for i, p := range s.CredentialsSchema.Providers {
				base := fmt.Sprintf("security.credentials_schema.providers.%d", i)
				if p.Type != "" {
					v.enum(base+".type", string(p.Type), enumStrings(schema.AllCredentialTypes))
				}
				if p.Scope != "" {
					v.enum(base+".scope", string(p.Scope), enumStrings(schema.AllCredentialScopes))
				}
				if p.Transport != "" {
					v.enum(base+".transport", string(p.Transport), enumStrings(schema.AllMCPTransports))
				}
				for j, f := range p.Fields {
					if f.Type != "" {
						v.enum(fmt.Sprintf("%s.fields.%d.type", base, j),
							string(f.Type), enumStrings(schema.AllCredentialFieldTypes))
					}
				}
			}
		}
	}

	if u := v.def.UI; u != nil {
		if u.Layout != "" {
			v.enum("ui.layout", string(u.Layout), enumStrings(schema.AllLayouts))
		}
		if u.Density != "" {
			v.enum("ui.density", string(u.Density), enumStrings(schema.AllDensities))
		}
	}
}

func (v *validator) enum(path, got string, allowed []string) {
	if got == "" {
		return
	}
	for _, a := range allowed {
		if got == a {
			return
		}
	}
	d := diagnostic.Errorf(diagnostic.CodeBadEnum, v.pos(path),
		"invalid value %q at %s (allowed: %s)", got, path, fmtList(allowed))
	if s, ok := suggest.Closest(got, allowed, 3); ok {
		d = d.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
	}
	v.bag.Add(d)
}

func modeStrings() []string { return enumStrings(schema.AllModes) }

func enumStrings[T ~string](src []T) []string {
	out := make([]string, len(src))
	for i, s := range src {
		out[i] = string(s)
	}
	return out
}
