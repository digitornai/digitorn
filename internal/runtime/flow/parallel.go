package flow

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

const defaultJoinTimeout = 60 * time.Second

type branchOutcome struct {
	idx   int
	res   execResult
	err   error
	fc    *fctx
}

// execParallel fans out each branch as a concurrent sub-path, joins per the
// configured policy, and merges branch contexts back. The parallel node's own
// routes are evaluated afterwards by the runner.
func (r *Runner) execParallel(ctx context.Context, node schema.FlowNode, fc *fctx, in runInput) (execResult, error) {
	if len(node.Branches) == 0 {
		return execResult{status: "completed"}, nil
	}

	joinType, needed, timeout := parseJoin(node.Join, len(node.Branches))

	pCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		var tCancel context.CancelFunc
		pCtx, tCancel = context.WithTimeout(pCtx, timeout)
		defer tCancel()
	}

	rs := &runState{maxIter: defaultMaxFlowIterations}
	resultCh := make(chan branchOutcome, len(node.Branches))
	var wg sync.WaitGroup
	wg.Add(len(node.Branches))

	for i, b := range node.Branches {
		branchCtx := fc.clone()
		go func(idx int, head string, bfc *fctx) {
			defer wg.Done()
			res, err := r.runPath(pCtx, head, bfc, in, rs)
			resultCh <- branchOutcome{idx: idx, res: res, err: err, fc: bfc}
		}(i, b.To, branchCtx)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	texts := make([]string, len(node.Branches))
	completed := 0
	var errs []string

	for out := range resultCh {
		fc.merge(out.fc)
		if out.err != nil {
			errs = append(errs, out.err.Error())
		} else {
			texts[out.idx] = out.res.text
			completed++
		}
		if completed >= needed && joinType != "all" {
			cancel()
			go func() {
				for range resultCh {
				}
			}()
			break
		}
	}

	combined := strings.Join(nonEmpty(texts), "\n\n")
	if joinType == "all" && len(errs) > 0 {
		return execResult{status: "errored", text: combined},
			errCombined(errs)
	}
	if completed == 0 && len(errs) > 0 {
		return execResult{status: "errored", text: combined}, errCombined(errs)
	}
	return execResult{status: "completed", text: combined}, nil
}

// parseJoin resolves the join policy against the Go schema (Type, Min, Timeout).
// Doc names: all (default), any/first, count/min. The Go field is Min.
func parseJoin(j *schema.FlowJoinConfig, branchCount int) (joinType string, needed int, timeout time.Duration) {
	joinType = "all"
	needed = branchCount
	timeout = defaultJoinTimeout
	if j == nil {
		return
	}
	switch j.Type {
	case "any", "first":
		joinType, needed = "any", 1
	case "count", "min":
		joinType = "count"
		needed = j.Min
		if needed < 1 {
			needed = 1
		}
		if needed > branchCount {
			needed = branchCount
		}
	case "all", "":
		joinType, needed = "all", branchCount
	}
	if j.Timeout > 0 {
		timeout = time.Duration(j.Timeout * float64(time.Second))
	}
	return
}

func matchErrorRoute(routes []schema.FlowErrorRoute, errMsg string) string {
	fallback := ""
	for _, rt := range routes {
		if rt.Default {
			if fallback == "" {
				fallback = rt.To
			}
			continue
		}
		if rt.Match == "" {
			continue
		}
		re, err := cachedRegexp(rt.Match)
		if err == nil && re.MatchString(errMsg) {
			return rt.To
		}
	}
	return fallback
}

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func errCombined(errs []string) error {
	return &joinError{msg: strings.Join(errs, "; ")}
}

type joinError struct{ msg string }

func (e *joinError) Error() string { return e.msg }
