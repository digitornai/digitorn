package schema

// HooksOrNil safely returns the runtime hooks slice, returning nil when the
// runtime block itself is absent. Keeps validation passes branch-free.
func (a *AppDefinition) RuntimeHooksOrNil() []Hook {
	if a == nil || a.Runtime == nil {
		return nil
	}
	return a.Runtime.Hooks
}
