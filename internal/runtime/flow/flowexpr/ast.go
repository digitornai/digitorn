package flowexpr

type node interface{ node() }

type litString struct{ v string }
type litNumber struct{ v float64 }
type litBool struct{ v bool }
type ref struct{ path []string }
type defaultSentinel struct{}

type binary struct {
	op   tokenKind
	l, r node
}

type unaryNot struct{ x node }

func (litString) node()       {}
func (litNumber) node()       {}
func (litBool) node()         {}
func (ref) node()             {}
func (defaultSentinel) node() {}
func (binary) node()          {}
func (unaryNot) node()        {}
