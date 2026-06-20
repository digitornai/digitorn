//go:build !treesitter

package filesystem

import "context"

// astSearch is a no-op without the treesitter build tag.
func (m *Module) astSearch(_ context.Context, _, _ string, _ int) []astHit {
	return nil
}
