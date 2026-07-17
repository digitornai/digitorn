package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
	pkgmodule "github.com/digitornai/digitorn/pkg/module"
)

type BlobPutter interface {
	Put(ctx context.Context, mime string, r io.Reader) (sessionstore.BlobRef, error)
}

type BusAdapter struct {
	Bus ports.ServiceBus

	Pipelines PipelineSource

	NowFn func() time.Time

	Blobs BlobPutter

	FileChangeNotifier tool.FileChangeNotifier

	ModuleConfigs ModuleConfigSource

	Embedder pkgmodule.Embedder
	Reranker pkgmodule.Reranker

	EventBus ports.EventBus

	AppsRoot string
}

type ModuleConfigSource interface {
	ModuleConfig(appID, userID, moduleID string) map[string]any
}

type ToolPipeline interface {
	Run(ctx context.Context, params []byte, terminal func(context.Context) (tool.Result, error)) (tool.Result, error)
}

type PipelineSource interface {
	PipelineFor(appID, moduleID string) ToolPipeline
}

var legacyWorkspaceFileTools = map[string]bool{
	"read": true, "write": true, "edit": true, "multi_edit": true,
	"glob": true, "grep": true, "delete": true,
}

func aliasLegacyToolModule(moduleID, action string) string {
	if moduleID == "workspace" && legacyWorkspaceFileTools[action] {
		return "filesystem"
	}
	return moduleID
}

func aliasMCPTool(moduleID, action string) (string, string) {
	if strings.HasPrefix(moduleID, "mcp_") {
		return "mcp", moduleID + "__" + action
	}
	return moduleID, action
}

func aliasPiecesTool(moduleID, action string) (string, string) {
	if strings.HasPrefix(moduleID, "ap_") {
		return "pieces", moduleID + "__" + action
	}
	return moduleID, action
}

func (a *BusAdapter) callBus(ctx context.Context, appID, moduleID, actionName string, raw []byte) (tool.Result, error) {
	terminal := func(ctx context.Context) (tool.Result, error) {
		return a.Bus.Call(ctx, moduleID, actionName, raw)
	}
	if a.Pipelines != nil {
		if p := a.Pipelines.PipelineFor(appID, moduleID); p != nil {
			return p.Run(ctx, raw, terminal)
		}
	}
	return terminal(ctx)
}

func NewBusAdapter(bus ports.ServiceBus) *BusAdapter {
	if bus == nil {
		return nil
	}
	return &BusAdapter{Bus: bus}
}

func (a *BusAdapter) Dispatch(ctx context.Context, call runtime.ToolInvocation) runtime.ToolOutcome {
	start := a.now()

	moduleID, actionName, ok := splitFQN(call.Name)
	if !ok {
		return errored(call.Name, "tool name must be of the form 'module.action'", start, a.now())
	}
	moduleID = aliasLegacyToolModule(moduleID, actionName)
	moduleID, actionName = aliasMCPTool(moduleID, actionName)
	moduleID, actionName = aliasPiecesTool(moduleID, actionName)

	raw, err := json.Marshal(call.Args)
	if err != nil {
		return errored(call.Name, "marshal args: "+err.Error(), start, a.now())
	}

	if a.Bus == nil {
		return errored(call.Name, "service bus is nil", start, a.now())
	}

	ctx = tool.WithIdentity(ctx, tool.Identity{
		AppID: call.AppID, SessionID: call.SessionID, UserID: call.UserID,
		AgentID: call.AgentID, ModuleID: moduleID, ToolName: actionName,
	})

	if a.ModuleConfigs != nil {
		if cfg := a.ModuleConfigs.ModuleConfig(call.AppID, call.UserID, moduleID); len(cfg) > 0 {
			ctx = pkgmodule.WithModuleConfig(ctx, cfg)
		}
	}
	if a.AppsRoot != "" && call.AppID != "" {
		ctx = pkgmodule.WithAppDir(ctx, filepath.Join(a.AppsRoot, call.AppID))
	}
	if a.Embedder != nil {
		ctx = pkgmodule.WithEmbedder(ctx, a.Embedder)
	}
	if a.Reranker != nil {
		ctx = pkgmodule.WithReranker(ctx, a.Reranker)
	}

	if a.FileChangeNotifier != nil {
		ctx = tool.WithFileChangeNotifier(ctx, a.FileChangeNotifier)
	}

	if a.EventBus != nil {
		ctx = tool.WithEventBus(ctx, a.EventBus)
	}

	res, busErr := a.callBus(ctx, call.AppID, moduleID, actionName, raw)
	end := a.now()

	if busErr != nil {
		msg := busErr.Error()
		if res.Error != "" && !strings.Contains(msg, res.Error) {
			msg = msg + ": " + res.Error
		}
		return errored(call.Name, msg, start, end)
	}

	if !res.Success {
		errMsg := res.Error
		if errMsg == "" {
			errMsg = "tool reported failure without error message"
		}
		return runtime.ToolOutcome{
			Status:     "errored",
			Error:      "tool=" + call.Name + ": " + errMsg,
			Parts:      a.partsFromResult(ctx, res),
			DurationMs: end.Sub(start).Milliseconds(),
			Diff:       res.Diff,
			Metadata:   res.Metadata,
		}
	}

	return runtime.ToolOutcome{
		Status:     "completed",
		Parts:      a.partsFromResult(ctx, res),
		DurationMs: end.Sub(start).Milliseconds(),
		Diff:       res.Diff,
		Metadata:   res.Metadata,
	}
}

func splitFQN(name string) (module, action string, ok bool) {
	dot := strings.IndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return "", "", false
	}
	return name[:dot], name[dot+1:], true
}

func errored(toolName, msg string, start, end time.Time) runtime.ToolOutcome {
	return runtime.ToolOutcome{
		Status:     "errored",
		Error:      "tool=" + toolName + ": " + msg,
		DurationMs: end.Sub(start).Milliseconds(),
	}
}

func (a *BusAdapter) partsFromResult(ctx context.Context, res tool.Result) []sessionstore.MessagePart {
	if len(res.OutputParts) == 0 {
		return []sessionstore.MessagePart{{Type: sessionstore.PartTypeText, Text: renderData(res.Data)}}
	}
	textPart := func(s string) sessionstore.MessagePart {
		return sessionstore.MessagePart{Type: sessionstore.PartTypeText, Text: s}
	}
	out := make([]sessionstore.MessagePart, 0, len(res.OutputParts))
	for _, op := range res.OutputParts {
		switch op.Kind {
		case tool.OutputText, "":
			if op.Text != "" {
				out = append(out, textPart(op.Text))
			}
		case tool.OutputImage, tool.OutputAudio, tool.OutputVideo, tool.OutputFile:
			if a.Blobs == nil || len(op.Bytes) == 0 {
				out = append(out, textPart(fmt.Sprintf("[%s %q (%d bytes) — not viewable here]", op.Kind, op.Name, len(op.Bytes))))
				continue
			}
			ref, err := a.Blobs.Put(ctx, op.Mime, bytes.NewReader(op.Bytes))
			if err != nil {
				out = append(out, textPart(fmt.Sprintf("[%s %q — could not be stored: %v]", op.Kind, op.Name, err)))
				continue
			}
			out = append(out, sessionstore.MessagePart{Type: blobPartType(op.Kind), Blob: &ref})
		default:
			if op.Text != "" {
				out = append(out, textPart(op.Text))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, textPart(renderData(res.Data)))
	}
	return out
}

func blobPartType(kind string) string {
	switch kind {
	case tool.OutputImage:
		return sessionstore.PartTypeImage
	case tool.OutputAudio:
		return sessionstore.PartTypeAudio
	case tool.OutputVideo:
		return sessionstore.PartTypeVideo
	default:
		return sessionstore.PartTypeFile
	}
}

func renderData(data any) string {
	if data == nil {
		return ""
	}
	switch v := data.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", data)
	}
	return string(b)
}

func (a *BusAdapter) now() time.Time {
	if a == nil || a.NowFn == nil {
		return time.Now()
	}
	return a.NowFn()
}

var _ runtime.ToolDispatcher = (*BusAdapter)(nil)
