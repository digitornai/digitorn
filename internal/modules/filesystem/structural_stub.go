//go:build !treesitter

package filesystem

import "context"

func (m *Module) astSearch(_ context.Context, _, _ string, _ int) []astHit {
	return nil
}
