package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// execResult is the outcome of running one node. Routing happens afterwards in
// the runner by evaluating node.Routes against the context.
type execResult struct {
	text      string
	status    string
	terminate bool
}

// executeNode runs a single node, recording its output into ctx. It does NOT
// decide the next node; the runner evaluates routes after this returns.
func (r *Runner) executeNode(ctx context.Context, node schema.FlowNode, fc *fctx, in runInput) (execResult, error) {
	switch node.Type {
	case "agent":
		return r.execAgent(ctx, node, fc, in)
	case "tool":
		return r.execTool(ctx, node, fc, in)
	case "parallel":
		return r.execParallel(ctx, node, fc, in)
	case "decision":
		return execResult{status: "completed", text: fc.lastText()}, nil
	case "approval":
		return r.execApproval(ctx, node, fc, in)
	case "terminal":
		return r.execTerminal(node, fc), nil
	default:
		return execResult{status: "errored"},
			fmt.Errorf("flow: unknown node type %q (node %q)", node.Type, node.ID)
	}
}

func (r *Runner) execAgent(ctx context.Context, node schema.FlowNode, fc *fctx, in runInput) (execResult, error) {
	if node.Agent == "" {
		return execResult{status: "errored"}, fmt.Errorf("flow: node %q: agent field required", node.ID)
	}
	task := r.agentTask(node, fc)
	result, err := r.deps.RunAgent(ctx, AgentSpec{
		AppID:         in.appID,
		ParentSession: in.sessionID,
		UserID:        in.userID,
		UserJWT:       in.userJWT,
		AgentID:       node.Agent,
		RunID:         r.idGen(),
		Task:          task,
		MemorySeed:    fc.interpolate(stringParam(node.Params, "memory_seed")),
	})
	if err != nil {
		return execResult{status: "errored"}, err
	}
	fc.recordAgent(node.ID, result.Content)
	if result.Status == "errored" {
		return execResult{status: "errored", text: result.Content},
			fmt.Errorf("flow: agent %q: %s", node.Agent, result.Error)
	}
	return execResult{status: "completed", text: result.Content}, nil
}

func (r *Runner) execTool(ctx context.Context, node schema.FlowNode, fc *fctx, in runInput) (execResult, error) {
	if node.Tool == "" {
		return execResult{status: "errored"}, fmt.Errorf("flow: node %q: tool field required", node.ID)
	}
	args := r.interpolateParams(node.Params, fc)
	outcome := r.deps.RunTool(ctx, ToolInvocation{
		CallID:    r.idGen(),
		Name:      node.Tool,
		Args:      args,
		AppID:     in.appID,
		UserID:    in.userID,
		SessionID: in.sessionID,
		UserJWT:   in.userJWT,
	})
	var text string
	for _, p := range outcome.Parts {
		text += p.Text
	}
	fc.recordTool(node.ID, text)
	if outcome.Status == "errored" {
		return execResult{status: "errored", text: text},
			fmt.Errorf("flow: tool %q: %s", node.Tool, outcome.Error)
	}
	return execResult{status: "completed", text: text}, nil
}

func (r *Runner) execApproval(ctx context.Context, node schema.FlowNode, fc *fctx, in runInput) (execResult, error) {
	if r.deps.Approvals == nil {
		return execResult{status: "errored"}, fmt.Errorf("flow: node %q: approval registry not wired", node.ID)
	}
	choices := normalizeChoices(node.Choices)
	requestID := r.idGen()
	message := fc.interpolate(node.Message)
	if message == "" {
		message = "Approval required."
	}

	pending := r.deps.Approvals.Arm(requestID)
	r.emitApprovalRequest(ctx, in, requestID, node.ID, message, choices)

	res := pending.Wait(ctx, approvalTimeout)
	choice := resolutionToChoice(res, choices)
	fc.recordApproval(node.ID, choice)
	return execResult{status: "completed", text: choice}, nil
}

func (r *Runner) execTerminal(node schema.FlowNode, fc *fctx) execResult {
	out := fc.interpolate(stringParam(node.Params, "output"))
	if out == "" {
		out = fc.lastText()
	}
	return execResult{status: "completed", text: out, terminate: true}
}

// agentTask derives the agent's user-turn input from the node params. The Go
// schema carries agent input under `params` (the doc's `input:` is a schema
// alias). Convention: an explicit `task`/`user_message` wins; otherwise the
// resolved params are JSON-encoded so a structured input still reaches the agent.
func (r *Runner) agentTask(node schema.FlowNode, fc *fctx) string {
	if s := stringParam(node.Params, "task"); s != "" {
		return fc.interpolate(s)
	}
	if s := stringParam(node.Params, "user_message"); s != "" {
		return fc.interpolate(s)
	}
	if len(node.Params) == 0 {
		return ""
	}
	resolved := r.interpolateParams(node.Params, fc)
	b, _ := json.Marshal(resolved)
	return string(b)
}

func (r *Runner) interpolateParams(params map[string]any, fc *fctx) map[string]any {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		if s, ok := v.(string); ok {
			out[k] = fc.interpolate(s)
		} else {
			out[k] = v
		}
	}
	return out
}

func stringParam(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	if s, ok := params[key].(string); ok {
		return s
	}
	return ""
}

func normalizeChoices(raw []any) []string {
	if len(raw) == 0 {
		return []string{"approve", "reject"}
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		} else {
			out = append(out, fmt.Sprintf("%v", v))
		}
	}
	return out
}

// resolutionToChoice maps an approval registry resolution to one of the node's
// declared choices. Approved → the first choice (conventionally "approve");
// any non-approval → the second choice (conventionally "reject").
func resolutionToChoice(res approvalResolution, choices []string) string {
	approve, reject := "approve", "reject"
	if len(choices) > 0 {
		approve = choices[0]
	}
	if len(choices) > 1 {
		reject = choices[1]
	}
	switch res.Result {
	case approvalResultApproved, approvalResultApprovedAlways:
		if c := strings.TrimSpace(res.Reason); c != "" && contains(choices, c) {
			return c
		}
		return approve
	default:
		if c := strings.TrimSpace(res.Reason); c != "" && contains(choices, c) {
			return c
		}
		return reject
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
