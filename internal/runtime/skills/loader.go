// Package skills implements the SkillLoader contract from
// internal/runtime/context/meta. A skill is a markdown file
// declared under `dev.skills[]` in the app YAML : the compiler
// reads each file's `path` at compile time and the bundle ships
// the content. At runtime the loader resolves a /command to the
// matching entry and returns its compiled content.
//
// Resolution per docs-site/language/21-skills.md :
//
//   - Match against `dev.skills[]` entries by `command` field.
//   - Leading "/" is accepted both with and without.
//   - When `path` content is missing from the bundle (compiler
//     should embed it), the loader falls back to reading from
//     {bundleDir}/{path} so dev workflows that bypass the
//     compiler still work.
//
// The loader is per-process ; one instance is enough for the
// whole daemon. It caches resolved entries by (appID, command)
// for the process lifetime.
package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
)

// BundleLoader is the production SkillLoader : resolves skills via
// the app's compiled definition (dev.skills[]).
type BundleLoader struct {
	// Apps resolves the app's runtime definition + bundle dir.
	Apps appmgr.Manager

	mu    sync.RWMutex
	cache map[string]meta.SkillEntry // key = appID + "|" + command
}

// New constructs a BundleLoader bound to the given app manager.
func New(apps appmgr.Manager) *BundleLoader {
	return &BundleLoader{
		Apps:  apps,
		cache: map[string]meta.SkillEntry{},
	}
}

// Load implements meta.SkillLoader. Returns the matching
// SkillEntry whose Content is the skill markdown body.
func (l *BundleLoader) Load(ctx context.Context, appID, command string) (meta.SkillEntry, error) {
	cmd := normalizeCommand(command)
	if cmd == "" {
		return meta.SkillEntry{}, errors.New("skills: command required")
	}

	key := appID + "|" + cmd
	l.mu.RLock()
	if v, ok := l.cache[key]; ok {
		l.mu.RUnlock()
		return v, nil
	}
	l.mu.RUnlock()

	entry, err := l.resolve(ctx, appID, cmd)
	if err != nil {
		return meta.SkillEntry{}, err
	}

	l.mu.Lock()
	l.cache[key] = entry
	l.mu.Unlock()
	return entry, nil
}

// resolve walks dev.skills[] for the matching command and reads
// the skill file. Returns an error when no entry matches OR the
// file isn't readable.
func (l *BundleLoader) resolve(ctx context.Context, appID, command string) (meta.SkillEntry, error) {
	if l.Apps == nil || appID == "" {
		return meta.SkillEntry{}, fmt.Errorf("skills: no app manager")
	}
	rt, err := l.Apps.Get(ctx, appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Dev == nil {
		return meta.SkillEntry{}, fmt.Errorf("skills: app %q has no dev.skills", appID)
	}
	for _, entry := range rt.Definition.Dev.Skills {
		if normalizeCommand(entry.Command) != command {
			continue
		}
		content, err := readSkillContent(rt.BundleDir, entry.Path)
		if err != nil {
			return meta.SkillEntry{}, fmt.Errorf("skills: %q: %w", entry.Command, err)
		}
		return meta.SkillEntry{
			Command:     prefixSlash(entry.Command),
			Description: entry.Description,
			Content:     content,
		}, nil
	}
	return meta.SkillEntry{}, fmt.Errorf("skills: %q not found in dev.skills", command)
}

// readSkillContent reads {bundleDir}/{path} with traversal
// protection. The path must stay inside the bundle directory.
func readSkillContent(bundleDir, path string) (string, error) {
	if bundleDir == "" {
		return "", errors.New("bundle dir not set")
	}
	if path == "" {
		return "", errors.New("skill path empty")
	}
	absRoot, err := filepath.Abs(bundleDir)
	if err != nil {
		return "", err
	}
	absSkill, err := filepath.Abs(filepath.Join(bundleDir, path))
	if err != nil {
		return "", err
	}
	// Resolve symlinks so a symlink planted inside the bundle can't point the
	// "contained" path outside it (the Rel check below is purely lexical). When
	// the target doesn't exist yet EvalSymlinks errors — keep the lexical form,
	// which still catches plain "../" traversal and the subsequent ReadFile
	// surfaces the not-found.
	if r, e := filepath.EvalSymlinks(absSkill); e == nil {
		absSkill = r
	}
	if r, e := filepath.EvalSymlinks(absRoot); e == nil {
		absRoot = r
	}
	rel, err := filepath.Rel(absRoot, absSkill)
	// Proper boundary check : reject the parent itself and anything under it,
	// but NOT a legitimate file whose name merely starts with ".." (e.g.
	// "..keep.md") — the old HasPrefix(rel, "..") rejected those too.
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("skill path %q escapes bundle dir", path)
	}
	data, err := os.ReadFile(absSkill)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// normalizeCommand strips a leading "/" so equality matches both
// "/commit" and "commit". Returns the canonical no-slash form,
// trimmed and lower-cased.
func normalizeCommand(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	return strings.ToLower(s)
}

// prefixSlash adds a leading "/" if missing. Used for echoing the
// canonical form back in the result.
func prefixSlash(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return s
}

// Compile-time guard.
var _ meta.SkillLoader = (*BundleLoader)(nil)
