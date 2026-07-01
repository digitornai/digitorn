// Package validate runs semantic checks on a compiled AppDefinition:
// mode gating, cycle detection, numeric bounds, regex/semver, duplicate IDs,
// brain auth, and grant/deny consistency.
package validate

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/parse"
	"github.com/digitornai/digitorn/internal/compiler/position"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func Check(file string, doc *yaml.Node, def *schema.AppDefinition, bag *diagnostic.Bag) {
	v := &validator{file: file, doc: doc, def: def, bag: bag}
	v.checkMode()
	v.checkComposerModes()
	v.checkBehavior()
	v.checkBounds()
	v.checkRegex()
	v.checkEnums()
	v.checkCycles()
	v.checkDuplicates()
	v.checkBrainAuth()
	v.checkGrantOverlap()
	v.checkFallback()
}

type validator struct {
	file string
	doc  *yaml.Node
	def  *schema.AppDefinition
	bag  *diagnostic.Bag
}

func (v *validator) pos(path string) position.Pos { return parse.LookupPos(v.file, v.doc, path) }

func (v *validator) errf(code diagnostic.Code, path string, format string, args ...any) {
	v.bag.Add(diagnostic.Errorf(code, v.pos(path), format, args...))
}

func (v *validator) warnf(code diagnostic.Code, path string, format string, args ...any) {
	v.bag.Add(diagnostic.Warningf(code, v.pos(path), format, args...))
}

func mode(def *schema.AppDefinition) schema.Mode {
	if def.Runtime == nil || def.Runtime.Mode == "" {
		return schema.ModeConversation
	}
	return def.Runtime.Mode
}

func fmtList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", s)
	}
	return out
}
