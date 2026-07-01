package skills

import (
	"context"

	"github.com/digitornai/digitorn/internal/runtime/context/meta"
)

// UserLoader resolves a per-user authored skill by command, scoped to
// (appID, userID). found=false (with nil error) means "no such user skill" —
// the layered loader then falls through to the app's bundled skills. The
// wiring seam (bootstrap) adapts the user-skill store to this shape so this
// package stays free of any persistence dependency.
type UserLoader interface {
	Load(ctx context.Context, appID, userID, command string) (meta.SkillEntry, bool, error)
}

// UserLoaderFunc adapts a function to UserLoader.
type UserLoaderFunc func(ctx context.Context, appID, userID, command string) (meta.SkillEntry, bool, error)

// Load calls the wrapped function.
func (f UserLoaderFunc) Load(ctx context.Context, appID, userID, command string) (meta.SkillEntry, bool, error) {
	return f(ctx, appID, userID, command)
}

// LayeredLoader is the production SkillLoader : it tries the user's authored
// skills first, then the app's bundled skills. This is the single skill
// registry the agent sees — one resolution path, two sources, user wins on a
// name clash (the user deliberately authored an override).
type LayeredLoader struct {
	User UserLoader   // optional ; nil = app skills only
	App  *BundleLoader // required
}

// NewLayered builds a LayeredLoader. user may be nil.
func NewLayered(user UserLoader, app *BundleLoader) *LayeredLoader {
	return &LayeredLoader{User: user, App: app}
}

// Load implements meta.SkillLoader. A user-layer error is non-fatal : we log
// nothing here and fall through to the app layer so a DB hiccup never hides a
// perfectly good bundled skill.
func (l *LayeredLoader) Load(ctx context.Context, appID, userID, command string) (meta.SkillEntry, error) {
	if l.User != nil && userID != "" {
		if entry, found, err := l.User.Load(ctx, appID, userID, command); err == nil && found {
			return entry, nil
		}
	}
	return l.App.Load(ctx, appID, userID, command)
}

var _ meta.SkillLoader = (*LayeredLoader)(nil)
