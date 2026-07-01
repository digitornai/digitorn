package validate

import (
	"regexp"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
)

var (
	reAppID     = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	reHexColor  = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	reSemver    = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}([-+][0-9A-Za-z.\-]+)?$`)
	reCronToken = regexp.MustCompile(`^(\*|\?|[0-9]+(-[0-9]+)?(\/[0-9]+)?(,[0-9]+(-[0-9]+)?(\/[0-9]+)?)*|\*\/[0-9]+|[A-Za-z]+(,[A-Za-z]+)*)$`)
)

func (v *validator) checkRegex() {
	a := v.def.App
	if a.AppID != "" && !reAppID.MatchString(a.AppID) {
		v.errf(diagnostic.CodeBadRegex, "app.app_id",
			"app_id must match ^[a-z][a-z0-9_-]*$ (got %q)", a.AppID)
	}
	if a.Color != "" && !reHexColor.MatchString(a.Color) {
		v.errf(diagnostic.CodeBadHexColor, "app.color",
			"color must be hex #RRGGBB (got %q)", a.Color)
	}
	if a.Version != "" && !reSemver.MatchString(a.Version) {
		v.errf(diagnostic.CodeBadSemver, "app.version",
			"version must be semver-ish (got %q)", a.Version)
	}

	if v.def.Runtime != nil {
		for i, t := range v.def.Runtime.Triggers {
			if t.Type == "cron" && t.Schedule != "" && !looksLikeCron(t.Schedule) {
				v.errf(diagnostic.CodeBadCron,
					"runtime.triggers."+itoa(i)+".schedule",
					"schedule does not look like a valid cron expression (got %q)", t.Schedule)
			}
		}
	}

	if v.def.Dev != nil {
		for i, s := range v.def.Dev.Skills {
			if s.Command != "" && s.Command[0] != '/' {
				v.errf(diagnostic.CodeBadRegex,
					"dev.skills."+itoa(i)+".command",
					"command must start with '/' (got %q)", s.Command)
			}
		}
	}

	if v.def.UI != nil {
		for i, s := range v.def.UI.SlashCommands {
			if s.Command != "" && s.Command[0] != '/' {
				v.errf(diagnostic.CodeBadRegex,
					"ui.slash_commands."+itoa(i)+".command",
					"command must start with '/' (got %q)", s.Command)
			}
		}
	}
}

func looksLikeCron(s string) bool {
	fields := splitFields(s)
	if n := len(fields); n != 5 && n != 6 {
		return false
	}
	for _, f := range fields {
		if !reCronToken.MatchString(f) {
			return false
		}
	}
	return true
}

func splitFields(s string) []string {
	out := []string{}
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(s[i])
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
