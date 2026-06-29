package flowexpr

import (
	"fmt"
	"strconv"
	"sync"
)

// Context resolves a dotted path (already split) to a value. A missing path
// returns (nil, false). Values are expected to be string, float64, bool, or nil.
type Context interface {
	Lookup(path []string) (any, bool)
}

// Program is a parsed, reusable expression. Compile once, evaluate many times.
type Program struct {
	root node
	src  string
}

var cache sync.Map // src string → *Program (or compileErr)

type compileErr struct{ err error }

// Compile parses src into a reusable Program. Results are memoised: the flow
// graph is bounded and validated, so the same when-expression compiles once.
func Compile(src string) (*Program, error) {
	if v, ok := cache.Load(src); ok {
		switch t := v.(type) {
		case *Program:
			return t, nil
		case compileErr:
			return nil, t.err
		}
	}
	root, err := parse(src)
	if err != nil {
		cache.Store(src, compileErr{err: err})
		return nil, err
	}
	p := &Program{root: root, src: src}
	cache.Store(src, p)
	return p, nil
}

// Eval evaluates the compiled program against ctx, returning a boolean.
// The `default` sentinel always evaluates true (catch-all route).
func (p *Program) Eval(ctx Context) (bool, error) {
	v, err := evalNode(p.root, ctx)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// EvalString is a convenience that compiles and evaluates in one call.
func EvalString(src string, ctx Context) (bool, error) {
	p, err := Compile(src)
	if err != nil {
		return false, err
	}
	return p.Eval(ctx)
}

// EvalValue evaluates the program to its raw value (string/number/bool/nil)
// instead of coercing to bool. Used by decision nodes, which switch routes on
// the value of `expr` rather than on a boolean.
func (p *Program) EvalValue(ctx Context) (any, error) {
	return evalNode(p.root, ctx)
}

// ValueToString renders an evaluated value the way decision-route matching
// compares it against a literal `when`.
func ValueToString(v any) string { return toString(v) }

// EvalValueString compiles src and evaluates it to its string value.
func EvalValueString(src string, ctx Context) (string, error) {
	p, err := Compile(src)
	if err != nil {
		return "", err
	}
	v, err := p.EvalValue(ctx)
	if err != nil {
		return "", err
	}
	return toString(v), nil
}

func evalNode(n node, ctx Context) (any, error) {
	switch t := n.(type) {
	case defaultSentinel:
		return true, nil
	case litString:
		return t.v, nil
	case litNumber:
		return t.v, nil
	case litBool:
		return t.v, nil
	case ref:
		v, _ := ctx.Lookup(t.path)
		return v, nil
	case unaryNot:
		v, err := evalNode(t.x, ctx)
		if err != nil {
			return nil, err
		}
		return !truthy(v), nil
	case binary:
		return evalBinary(t, ctx)
	}
	return nil, fmt.Errorf("flowexpr: unknown node %T", n)
}

func evalBinary(b binary, ctx Context) (any, error) {
	switch b.op {
	case tAnd:
		l, err := evalNode(b.l, ctx)
		if err != nil {
			return nil, err
		}
		if !truthy(l) {
			return false, nil
		}
		r, err := evalNode(b.r, ctx)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	case tOr:
		l, err := evalNode(b.l, ctx)
		if err != nil {
			return nil, err
		}
		if truthy(l) {
			return true, nil
		}
		r, err := evalNode(b.r, ctx)
		if err != nil {
			return nil, err
		}
		return truthy(r), nil
	}

	l, err := evalNode(b.l, ctx)
	if err != nil {
		return nil, err
	}
	r, err := evalNode(b.r, ctx)
	if err != nil {
		return nil, err
	}

	switch b.op {
	case tEq:
		return valuesEqual(l, r), nil
	case tNeq:
		return !valuesEqual(l, r), nil
	case tLt, tGt, tLte, tGte:
		lf, lok := toNumber(l)
		rf, rok := toNumber(r)
		if !lok || !rok {
			return false, nil
		}
		switch b.op {
		case tLt:
			return lf < rf, nil
		case tGt:
			return lf > rf, nil
		case tLte:
			return lf <= rf, nil
		case tGte:
			return lf >= rf, nil
		}
	}
	return nil, fmt.Errorf("flowexpr: unsupported operator")
}

// valuesEqual compares two values. If both are numeric, compare numerically;
// otherwise compare by string form so `category == 'refund'` works regardless
// of whether the context value is a string or another scalar.
func valuesEqual(a, b any) bool {
	an, aok := toNumber(a)
	bn, bok := toNumber(b)
	if aok && bok {
		return an == bn
	}
	if ab, ok := a.(bool); ok {
		if bb, ok2 := b.(bool); ok2 {
			return ab == bb
		}
	}
	return toString(a) == toString(b)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	}
	return true
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	}
	return fmt.Sprintf("%v", v)
}
