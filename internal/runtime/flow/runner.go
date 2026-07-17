package flow

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/flow/flowexpr"
	"github.com/digitornai/digitorn/internal/runtime/policy/approval"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const (
	defaultMaxFlowIterations = 1000
	approvalTimeout          = 30 * time.Minute
	endSentinel              = "end"
)

type approvalResolution = approval.Resolution

const (
	approvalResultApproved       = approval.ResultApproved
	approvalResultApprovedAlways = approval.ResultApprovedAlways
)

type Deps struct {
	Sessions  SessionSink
	RunAgent  func(ctx context.Context, spec AgentSpec) (AgentResult, error)
	RunTool   func(ctx context.Context, inv ToolInvocation) ToolOutcome
	Approvals *approval.Registry
	Logger    *slog.Logger
}

type SessionSink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

type runInput struct {
	appID        string
	sessionID    string
	userID       string
	userJWT      string
	turnID       string
	event        map[string]any
	secretLookup func(key string) (string, bool)
}

func RunInput(appID, sessionID, userID, userJWT, turnID string) runInput {
	return runInput{appID: appID, sessionID: sessionID, userID: userID, userJWT: userJWT, turnID: turnID}
}

func (in runInput) WithEvent(event map[string]any) runInput {
	in.event = event
	return in
}

func (in runInput) WithSecretLookup(fn func(key string) (string, bool)) runInput {
	in.secretLookup = fn
	return in
}

type runState struct {
	visits  atomic.Int64
	maxIter int
}

type Runner struct {
	deps     Deps
	nodeByID map[string]*schema.FlowNode
	flowID   string
	idGen    func() string
}

func New(flow *schema.FlowConfig, deps Deps, idGen func() string) *Runner {
	nodeByID := make(map[string]*schema.FlowNode, len(flow.Nodes))
	for i := range flow.Nodes {
		n := &flow.Nodes[i]
		nodeByID[n.ID] = n
	}
	flowID := flow.ID
	if flowID == "" {
		flowID = "flow"
	}
	return &Runner{deps: deps, nodeByID: nodeByID, flowID: flowID, idGen: idGen}
}

func (r *Runner) Run(ctx context.Context, flow *schema.FlowConfig, in runInput) (*FlowResult, error) {
	r.emit(ctx, in, sessionstore.EventFlowStarted, "", "", "running", "", "", 0)

	fc := newContext(in.event, in.secretLookup)
	rs := &runState{maxIter: flow.MaxIterations}
	if rs.maxIter <= 0 {
		rs.maxIter = defaultMaxFlowIterations
	}

	entry := flow.Entry
	if entry == "" && len(flow.Nodes) > 0 {
		entry = flow.Nodes[0].ID
	}

	out, finalErr := r.runPath(ctx, entry, fc, in, rs)

	status, errMsg := "completed", ""
	if finalErr != nil {
		status, errMsg = "errored", finalErr.Error()
	}
	r.emit(ctx, in, sessionstore.EventFlowEnded, "", "", status, "", errMsg, int(rs.visits.Load()))

	if finalErr != nil {
		return nil, finalErr
	}
	return &FlowResult{Content: out.text}, nil
}

func (r *Runner) runPath(ctx context.Context, startID string, fc *fctx, in runInput, rs *runState) (execResult, error) {
	currentID := startID
	var last execResult

	for currentID != "" && currentID != endSentinel {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		if rs.visits.Add(1) > int64(rs.maxIter) {
			r.emit(ctx, in, sessionstore.EventFlowNodeEnd, currentID, "", "max_iterations", "",
				fmt.Sprintf("max_iterations (%d) reached; path forced to end", rs.maxIter), rs.maxIter)
			return last, nil
		}

		node, ok := r.findNode(currentID)
		if !ok {
			return last, fmt.Errorf("flow: node %q not found", currentID)
		}

		iter := int(rs.visits.Load())
		r.emit(ctx, in, sessionstore.EventFlowNodeStart, node.ID, node.Type, "running", "", "", iter)

		res, execErr := r.execWithRetry(ctx, *node, fc, in, iter)
		last = res

		if execErr != nil {
			r.emit(ctx, in, sessionstore.EventFlowNodeEnd, node.ID, node.Type, "errored", res.text, execErr.Error(), iter)
			fc.recordError(node.ID, node.Type, execErr.Error())
			to := matchErrorRoute(node.OnError, execErr.Error())
			if to == "" {
				return last, execErr
			}
			if to == endSentinel {
				return last, nil
			}
			currentID = to
			continue
		}

		r.emit(ctx, in, sessionstore.EventFlowNodeEnd, node.ID, node.Type, "completed", res.text, "", iter)

		if res.terminate {
			return last, nil
		}

		next, rErr := r.nextNode(*node, fc)
		if rErr != nil {
			return last, rErr
		}
		currentID = next
	}
	return last, nil
}

func (r *Runner) nextNode(node schema.FlowNode, fc *fctx) (string, error) {
	if len(node.Routes) == 0 {
		return "", nil
	}
	if node.Type == "decision" {
		return r.decisionRoute(node, fc)
	}
	for _, rt := range node.Routes {
		if isCatchAll(rt) {
			return rt.To, nil
		}
		ok, err := flowexpr.EvalString(rt.When, fc)
		if err != nil {
			return "", fmt.Errorf("flow: node %q route %q: %w", node.ID, rt.When, err)
		}
		if ok {
			return rt.To, nil
		}
	}
	return "", nil
}

func (r *Runner) decisionRoute(node schema.FlowNode, fc *fctx) (string, error) {
	value := ""
	if node.Expr != "" {
		v, err := flowexpr.EvalValueString(node.Expr, fc)
		if err != nil {
			return "", fmt.Errorf("flow: decision %q expr %q: %w", node.ID, node.Expr, err)
		}
		value = v
	} else {
		value = fc.lastText()
	}
	var fallback string
	for _, rt := range node.Routes {
		if isCatchAll(rt) {
			if fallback == "" {
				fallback = rt.To
			}
			continue
		}
		if rt.When == value {
			return rt.To, nil
		}
	}
	return fallback, nil
}

func isCatchAll(rt schema.FlowRoute) bool {
	return rt.Default || rt.When == "" || rt.When == "default"
}

func (r *Runner) findNode(id string) (*schema.FlowNode, bool) {
	n, ok := r.nodeByID[id]
	return n, ok
}

func (r *Runner) emit(ctx context.Context, in runInput, evType sessionstore.EventType, nodeID, nodeType, status, output, errMsg string, iteration int) {
	if r.deps.Sessions == nil {
		return
	}
	_, _ = r.deps.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:          evType,
		SessionID:     in.sessionID,
		AppID:         in.appID,
		UserID:        in.userID,
		CorrelationID: in.turnID,
		Flow: &sessionstore.FlowPayload{
			FlowID:    r.flowID,
			NodeID:    nodeID,
			NodeType:  nodeType,
			Status:    status,
			Output:    output,
			Error:     errMsg,
			Iteration: iteration,
		},
	})
}

func (r *Runner) emitApprovalRequest(ctx context.Context, in runInput, requestID, nodeID, message string, choices []string) {
	if r.deps.Sessions == nil {
		return
	}
	_, _ = r.deps.Sessions.AppendDurable(ctx, sessionstore.Event{
		Type:          sessionstore.EventApprovalRequest,
		SessionID:     in.sessionID,
		AppID:         in.appID,
		UserID:        in.userID,
		CorrelationID: in.turnID,
		Approval: &sessionstore.ApprovalPayload{
			ID:      requestID,
			Kind:    "flow_node",
			Status:  "pending",
			Reason:  message,
			Payload: map[string]any{"node_id": nodeID, "choices": choices},
		},
	})
}
