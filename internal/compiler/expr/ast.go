package expr

type Expr interface{ exprNode() }

type Literal struct {
	Value string
}

type Ref struct {
	Namespace string
	Path      []string
}

type Include struct {
	Path string
}

type Fallback struct {
	Alternatives []Expr
}

func (Literal) exprNode()  {}
func (Ref) exprNode()      {}
func (Include) exprNode()  {}
func (Fallback) exprNode() {}
