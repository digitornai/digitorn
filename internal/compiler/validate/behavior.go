package validate

import (
	"fmt"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

var knownBehaviorProfiles = map[string]struct{}{
	"dev": {}, "coding": {}, "research": {}, "data": {}, "creative": {}, "assistant": {},
}

// checkBehavior validates security.behavior : rule when/action enums (a wrong
// value silently never fires at runtime), the classifier frequency enum + its
// numeric bounds, and a soft warning on an unknown profile (which would yield
// no rules). Custom JSON profiles (resolved {{behavior.X}} placeholders, which
// start with "{") are left alone.
func (v *validator) checkBehavior() {
	if v.def.Security == nil || v.def.Security.Behavior == nil {
		return
	}
	b := v.def.Security.Behavior

	if b.Profile != "" && !strings.HasPrefix(b.Profile, "{") {
		if _, ok := knownBehaviorProfiles[b.Profile]; !ok {
			v.warnf(diagnostic.CodeUnknownEnumHint, "security.behavior.profile",
				"unknown behavior profile %q (built-ins: dev, coding, research, data, creative, assistant); it resolves to no rules unless defined in a behavior/*.yaml file", b.Profile)
		}
	}

	for i, rd := range b.RuleDefinitions {
		base := fmt.Sprintf("security.behavior.rule_definitions.%d", i)
		if rd.When != "" {
			v.enum(base+".when", string(rd.When), enumStrings(schema.AllRuleWhens))
		}
		if rd.Action != "" {
			v.enum(base+".action", string(rd.Action), enumStrings(schema.AllRuleActions))
		}
	}

	if c := b.Classifier; c != nil {
		if c.Frequency != "" {
			v.enum("security.behavior.classifier.frequency",
				string(c.Frequency), enumStrings(schema.AllClassifierFrequencies))
		}
		// 0 is the "use the documented default" sentinel for these (the
		// runtime falls back to 3 / 15 / 5), so only negatives are invalid.
		if c.FrequencyN < 0 {
			v.errf(diagnostic.CodeOutOfRange, "security.behavior.classifier.frequency_n",
				"frequency_n must be >= 0 (0 = default 3) (got %d)", c.FrequencyN)
		}
		if c.Timeout < 0 {
			v.errf(diagnostic.CodeOutOfRange, "security.behavior.classifier.timeout",
				"timeout must be >= 0 seconds (0 = default 15) (got %d)", c.Timeout)
		}
		if c.MaxDirectives < 0 {
			v.errf(diagnostic.CodeOutOfRange, "security.behavior.classifier.max_directives",
				"max_directives must be >= 0 (0 = default 5) (got %d)", c.MaxDirectives)
		}
		if c.Context != nil && c.Context.HistoryDepth < 0 {
			v.errf(diagnostic.CodeOutOfRange, "security.behavior.classifier.context.history_depth",
				"history_depth must be >= 0 (got %d)", c.Context.HistoryDepth)
		}
	}
}
