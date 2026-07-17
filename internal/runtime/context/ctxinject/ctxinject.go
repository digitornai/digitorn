package ctxinject

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

type Data struct {
	User    map[string]any
	App     map[string]any
	Agent   map[string]any
	Session map[string]any
	Env     map[string]any
	Now     time.Time
}

func (d Data) bag() map[string]any {
	b := map[string]any{
		"user":    d.User,
		"app":     d.App,
		"agent":   d.Agent,
		"session": d.Session,
		"env":     d.Env,
	}
	if !d.Now.IsZero() {
		b["date"] = d.Now.Format("2006-01-02")
		b["time"] = d.Now.Format("15:04")
		b["datetime"] = d.Now.Format("2006-01-02 15:04")
		b["weekday"] = d.Now.Weekday().String()
	}
	return b
}

var placeholder = regexp.MustCompile(`\{\{\s*([\w.]+)\s*\}\}`)

func Merge(app, agent *schema.ContextBlock) []schema.ContextSection {
	var out []schema.ContextSection
	idx := map[string]int{}
	add := func(s schema.ContextSection) {
		if s.ID != "" {
			if i, ok := idx[s.ID]; ok {
				out[i] = s
				return
			}
			idx[s.ID] = len(out)
		}
		out = append(out, s)
	}
	if app != nil {
		for _, s := range app.Sections {
			add(s)
		}
	}
	if agent != nil {
		for _, s := range agent.Sections {
			add(s)
		}
	}
	return out
}

func Render(sections []schema.ContextSection, d Data) string {
	if len(sections) == 0 {
		return ""
	}
	bag := d.bag()
	type block struct {
		prio, idx int
		text      string
	}
	var blocks []block
	for i, s := range sections {
		if w := strings.TrimSpace(s.When); w != "" && !evalWhen(bag, w) {
			continue
		}
		body := strings.TrimRight(sectionBody(s, d, bag), "\n")
		if strings.TrimSpace(body) == "" {
			continue
		}
		text := body
		if t := strings.TrimSpace(s.Title); t != "" {
			text = "# " + t + "\n" + body
		}
		if isExternalSection(s) {
			text = "<system-reminder>\n" + text + "\n</system-reminder>"
		}
		if s.Writable {
			text += "\n\n" + writableDirective(s)
		}
		blocks = append(blocks, block{s.Priority, i, text})
	}
	sort.SliceStable(blocks, func(a, b int) bool {
		if blocks[a].prio != blocks[b].prio {
			return blocks[a].prio < blocks[b].prio
		}
		return blocks[a].idx < blocks[b].idx
	})
	parts := make([]string, len(blocks))
	for i, bl := range blocks {
		parts[i] = bl.text
	}
	return strings.Join(parts, "\n\n")
}

func writableDirective(s schema.ContextSection) string {
	target := strings.TrimSpace(s.Dir)
	if target == "" {
		target = strings.TrimSpace(s.File)
	}
	if target == "" && len(s.Files) > 0 {
		target = filepath.Dir(strings.TrimSpace(s.Files[0]))
	}
	return fileMemoryDirectiveFor(target)
}

func isExternalSection(s schema.ContextSection) bool {
	return s.File != "" || len(s.Files) > 0 || s.Dir != ""
}

func sectionBody(s schema.ContextSection, d Data, bag map[string]any) string {
	if b := strings.TrimSpace(s.Builtin); b != "" {
		if fn, ok := builtins[strings.ToLower(b)]; ok {
			return fn(d)
		}
		return ""
	}
	workdir := toString(d.Session["workdir"])
	if paths := sectionFiles(s, bag); len(paths) > 0 {
		return renderFiles(paths, s.Optional, workdir)
	}
	if dir := strings.TrimSpace(interp(s.Dir, bag)); dir != "" {
		absDir := dir
		if !filepath.IsAbs(absDir) && workdir != "" {
			absDir = filepath.Join(workdir, absDir)
		}
		if strings.Contains(strings.ToLower(filepath.Base(absDir)), "memory") {
			return renderDirBudget(absDir)
		}
		return renderDir(dir, s.Optional, workdir)
	}
	if s.Template != "" {
		return interp(s.Template, bag)
	}
	return s.Text
}

const maxFileBytes = 100 * 1024

func sectionFiles(s schema.ContextSection, bag map[string]any) []string {
	var out []string
	if f := strings.TrimSpace(interp(s.File, bag)); f != "" {
		out = append(out, f)
	}
	for _, f := range s.Files {
		if f = strings.TrimSpace(interp(f, bag)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func renderFiles(paths []string, optional bool, workdir string) string {
	multi := len(paths) > 1
	var parts []string
	for _, p := range paths {
		content, err := readFile(p, workdir)
		if err != nil {
			if !optional {
				parts = append(parts, "[file: "+p+" — "+err.Error()+"]")
			}
			continue
		}
		if multi {
			content = "## " + filepath.Base(p) + "\n" + content
		}
		parts = append(parts, content)
	}
	return strings.Join(parts, "\n\n")
}

const memoryDirBudget = 4000

func typePriority(name string) int {
	switch {
	case strings.HasPrefix(name, "feedback_"):
		return 0
	case strings.HasPrefix(name, "project_"):
		return 1
	case strings.HasPrefix(name, "user_"):
		return 2
	case strings.HasPrefix(name, "reference_"):
		return 3
	default:
		return 4
	}
}

func renderDirBudget(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	type fileInfo struct {
		name    string
		modTime time.Time
		size    int64
		isIndex bool
	}

	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			name:    e.Name(),
			modTime: info.ModTime(),
			size:    info.Size(),
			isIndex: strings.EqualFold(e.Name(), "MEMORY.md"),
		})
	}
	if len(files) == 0 {
		return ""
	}

	sort.SliceStable(files, func(i, j int) bool {
		if files[i].isIndex != files[j].isIndex {
			return files[i].isIndex
		}
		pi, pj := typePriority(files[i].name), typePriority(files[j].name)
		if pi != pj {
			return pi < pj
		}
		return files[i].modTime.After(files[j].modTime)
	})

	budget := memoryDirBudget
	var parts []string
	skipped := 0

	for _, fi := range files {
		if budget <= 0 {
			skipped++
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, fi.name))
		if err != nil {
			continue
		}
		text := string(content)
		if len(text) > budget && !fi.isIndex {
			skipped++
			continue
		}
		if len(text) > budget {
			text = text[:budget]
			if i := strings.LastIndexByte(text, '\n'); i > 0 {
				text = text[:i]
			}
			text += "\n[… truncated]"
		}
		header := "## " + fi.name
		entry := header + "\n" + text
		parts = append(parts, entry)
		budget -= len(entry)
	}

	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("[%d memory file(s) not injected — budget exhausted. Use filesystem.read to load them.]", skipped))
	}
	return strings.Join(parts, "\n\n")
}

func renderDir(dir string, optional bool, workdir string) string {
	if !filepath.IsAbs(dir) && workdir != "" {
		dir = filepath.Join(workdir, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if optional {
			return ""
		}
		return "[dir: " + dir + " — " + err.Error() + "]"
	}
	var index, rest []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		if strings.EqualFold(e.Name(), "MEMORY.md") {
			index = append(index, e.Name())
		} else {
			rest = append(rest, e.Name())
		}
	}
	sort.Strings(rest)
	paths := append(index, rest...)
	if len(paths) == 0 {
		return ""
	}
	return renderFiles(paths, optional, dir)
}

func readFile(path, workdir string) (string, error) {
	if !filepath.IsAbs(path) && workdir != "" {
		path = filepath.Join(workdir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(b) > maxFileBytes {
		s := string(b[:maxFileBytes])
		if i := strings.LastIndexByte(s, '\n'); i > 0 {
			s = s[:i]
		}
		return s + "\n[… truncated]", nil
	}
	return string(b), nil
}

func interp(tmpl string, bag map[string]any) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		v, _ := resolve(bag, placeholder.FindStringSubmatch(m)[1])
		return toString(v)
	})
}

func resolve(bag map[string]any, path string) (any, bool) {
	var cur any = bag
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, ", ")
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, toString(e))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(x)
	}
}

func evalWhen(bag map[string]any, expr string) bool {
	for _, op := range []string{">=", "<=", "!=", ">", "<", "=="} {
		i := strings.Index(expr, op)
		if i <= 0 {
			continue
		}
		path := strings.TrimSpace(expr[:i])
		rhs := strings.TrimSpace(expr[i+len(op):])
		val, ok := resolve(bag, path)
		if !ok {
			return false
		}
		return compareValues(toString(val), op, rhs)
	}
	return truthy(resolve(bag, expr))
}

func compareValues(lhs, op, rhs string) bool {
	lf, lerr := strconv.ParseFloat(lhs, 64)
	rf, rerr := strconv.ParseFloat(rhs, 64)
	if lerr == nil && rerr == nil {
		switch op {
		case ">":
			return lf > rf
		case ">=":
			return lf >= rf
		case "<":
			return lf < rf
		case "<=":
			return lf <= rf
		case "==":
			return lf == rf
		case "!=":
			return lf != rf
		}
	}
	switch op {
	case "==":
		return lhs == rhs
	case "!=":
		return lhs != rhs
	case ">":
		return lhs > rhs
	case ">=":
		return lhs >= rhs
	case "<":
		return lhs < rhs
	case "<=":
		return lhs <= rhs
	}
	return false
}

func truthy(v any, ok bool) bool {
	if !ok || v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case bool:
		return x
	case []string:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	default:
		return true
	}
}
