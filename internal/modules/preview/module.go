package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

var (
	// ErrNoPreview means nothing is currently rendering this session's app: the
	// user has no preview open, or the page died before it could answer.
	ErrNoPreview = errors.New("no live preview for this session")
	// ErrBusy means too many commands are already queued for the page.
	ErrBusy = errors.New("preview is already handling other commands")
)

// shared is the process-wide store. The HTTP layer that receives the page's
// reports and the tool the agent calls live in different packages, so they meet
// here rather than over a wire.
var shared = NewStore()

// Shared exposes the store to the daemon's HTTP layer.
func Shared() *Store { return shared }

// Module lets the agent watch and drive the app it just built, in the browser
// where that app actually runs.
//
// Everything it returns is scoped to the caller's own session, resolved from the
// runtime identity rather than from a parameter, so one session can never
// observe or steer another's preview.
type Module struct {
	module.Base
	seq atomic.Uint64
}

func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          "preview",
		Version:     "2.0.0",
		Description: "See and drive the running app in the preview pane.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}

	m.RegisterTool(module.Tool{
		Name: "inspect",
		Description: "Look at your app actually RUNNING in the user's preview, and optionally interact with it first. " +
			"A successful build only proves the code compiles; this proves the app works. Use it after building, whenever the user says something is broken, and to test a flow end to end. " +
			"Returns whether the page rendered or came up blank, the route on screen, the runtime failures it hit (crashes, rejected promises, console errors, with file and line), the network calls that FAILED (a 404 or a blocked request is the usual reason data never shows up, and nothing in the console says so), whatever the code printed with console.log, the screen size in use, the visible text, and the elements you can act on, each with a `ref`. " +
			"To TEST a flow, pass `actions`: they run in order against the live page and the resulting state comes back, so acting and checking are one call — navigate to /signup, type an email, click Submit, and read whether the confirmation appeared. " +
			"Refs come from the elements of the LAST inspect and only hold until the page re-renders. Inside a sequence — type, then click — target by `match` (the visible label) rather than by ref, otherwise the second action fails on an element that no longer exists. Runtime errors are cleared once reported, so each call returns what happened since you last looked.",
		Params: []tool.ParamSpec{
			{
				Name: "actions",
				Type: "array",
				Description: "Interactions to run on the live page before reading it back, in order. Each is an object: " +
					"{do:\"navigate\", url:\"/signup\"} to go to a route of the app; " +
					"{do:\"click\", ref:\"e12\"} to click an element, or {do:\"click\", match:\"S'inscrire\"} to click it by its visible label — prefer `match` in a sequence, because a ref stops resolving the moment the page re-renders; " +
					"{do:\"type\", ref:\"e7\", text:\"a@b.com\"} to fill a field (fires the events React and Vue forms listen for); " +
					"{do:\"press\", key:\"enter\"} to press a key, which submits a form far more reliably than clicking its button; " +
					"{do:\"hover\", ref:\"e3\"} to open a menu that appears on mouse-over; " +
					"{do:\"check\", ref:\"e9\"} for a checkbox or a radio — use this rather than click, it ticks the box and fires the events forms listen for; " +
					"{do:\"select\", ref:\"e4\", text:\"France\"} to choose in a dropdown by its visible label; " +
					"{do:\"scroll\", text:\"bottom\"} or {do:\"scroll\", ref:\"e20\"} to reach what is below the fold; " +
					"{do:\"viewport\", text:\"mobile\"} to switch the preview to a phone, tablet or desktop width — the only way to actually verify the app is usable on a phone; " +
					"{do:\"wait_for\", text:\"Merci\"} to wait until some text appears (up to 8s), which is far more reliable than guessing a delay; " +
					"{do:\"detail\", ref:\"e12\"} when an element is on screen but does not behave — you get its attributes, its computed styles, whether it is disabled, and whether something else is sitting on top of it; " +
					"{do:\"wait\", ms:800} to let an animation or a fetch settle. " +
					"Omit it entirely to just look without touching anything.",
			},
			{
				Name:        "full",
				Type:        "boolean",
				Description: "Return everything: every element, the whole page text. Off by default — an unchanged screen answers with a note instead of the payload you already have, and a busy page shows the elements you are most likely to act on rather than all of them. Set it when you genuinely need the rest.",
				Default:     false,
			},
			{
				Name:        "text",
				Type:        "boolean",
				Description: "Include the page's visible text (default true). Set false when you only need the elements and the errors.",
				Default:     true,
			},
		},
		RiskLevel: tool.RiskLow,
		Tags:      []string{"preview", "test", "debug"},
		Aliases:   []string{"inspect preview", "check app", "tester l'application"},
		CLILabel:  "Inspect preview",
		Handler:   m.inspect,
	})

	return m
}

type inspectParams struct {
	Actions []actionParam `json:"actions"`
	Text    *bool         `json:"text"`
	Full    bool          `json:"full"`
}

type actionParam struct {
	Do  string `json:"do"`
	Ref string `json:"ref"`
	// Match targets an element by its visible label instead of by ref. A ref
	// belongs to the snapshot it came from: the moment the page re-renders —
	// which typing into a live field does — it points at a node that no longer
	// exists. A label survives that.
	Match string `json:"match"`
	Role  string `json:"role"`
	Text  string `json:"text"`
	Key   string `json:"key"`
	URL   string `json:"url"`
	MS    int    `json:"ms"`
}

func (m *Module) inspect(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p inspectParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return tool.Result{Success: false, Error: "invalid parameters: " + err.Error()}, nil
		}
	}

	id, ok := tool.IdentityFromContext(ctx)
	if !ok || id.AppID == "" || id.SessionID == "" {
		return tool.Result{Success: false, Error: "preview is only available inside a session"}, nil
	}
	app, session := id.AppID, id.SessionID

	if !shared.Live(app, session) {
		if _, seen := shared.Observe(app, session); !seen {
			return tool.Result{
				Success: false,
				Error:   "No preview is running for this session. Build the app first, and make sure the Preview is open.",
			}, nil
		}
	}

	snap, err := m.run(ctx, app, session, p.Actions)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoPreview):
			return tool.Result{
				Success: false,
				Error:   "The preview did not respond — it is closed, still loading, or crashed on load. Run inspect with no actions to read the errors.",
			}, nil
		case errors.Is(err, ErrBusy):
			return tool.Result{Success: false, Error: ErrBusy.Error()}, nil
		default:
			return tool.Result{Success: false, Error: err.Error()}, nil
		}
	}

	// Trim before answering. The agent inspects after every action, so handing
	// back a page it already holds is what burns its context.
	fp := fingerprint(snap)
	unchanged := shared.SwapSent(app, session, fp) == fp
	wantText := p.Text == nil || *p.Text
	summary := summarize(snap)
	shown, note := present(snap, unchanged, p.Full, wantText)
	shared.ClearErrors(app, session)

	meta := map[string]any{"summary": summary, "errors": len(shown.Errors)}
	if note != "" {
		meta["note"] = note
	}
	return tool.Result{Success: true, Data: shown, Metadata: meta}, nil
}

// run executes the requested actions in order, stopping at the first one the
// page could not carry out. The snapshot returned is the state after the last
// action that ran, which is what the agent needs to judge the outcome.
func (m *Module) run(ctx context.Context, app, session string, actions []actionParam) (Snapshot, error) {
	if len(actions) == 0 {
		return shared.Submit(ctx, app, session, Command{ID: m.nextID(), Do: "observe"})
	}

	var snap Snapshot
	for i, a := range actions {
		cmd, err := toCommand(m.nextID(), a)
		if err != nil {
			return snap, fmt.Errorf("actions[%d]: %w", i, err)
		}
		snap, err = shared.Submit(ctx, app, session, cmd)
		if err != nil {
			return snap, err
		}
	}
	return snap, nil
}

func toCommand(id string, a actionParam) (Command, error) {
	do := strings.ToLower(strings.TrimSpace(a.Do))
	c := Command{ID: id, Do: do, Ref: a.Ref, Text: a.Text, Key: a.Key, URL: a.URL, Timeout: a.MS}
	c.TextMatch = strings.TrimSpace(a.Match)
	c.Role = strings.TrimSpace(a.Role)
	targeted := strings.TrimSpace(a.Ref) != "" || c.TextMatch != ""
	switch do {
	case "navigate":
		if strings.TrimSpace(a.URL) == "" {
			return c, errors.New("navigate needs a url")
		}
	case "click":
		if !targeted {
			return c, errors.New("click needs a ref from the last inspect, or match with the element's visible label")
		}
	case "type":
		if !targeted {
			return c, errors.New("type needs a ref from the last inspect, or match with the field's visible label")
		}
	case "press":
		if strings.TrimSpace(a.Key) == "" {
			return c, errors.New("press needs a key")
		}
	case "detail":
		if !targeted {
			return c, errors.New("detail needs a ref from the last inspect, or match with the visible label")
		}
	case "hover", "check":
		if !targeted {
			return c, fmt.Errorf("%s needs a ref from the last inspect, or match with the visible label", do)
		}
	case "select":
		if !targeted || strings.TrimSpace(a.Text) == "" {
			return c, errors.New("select needs a ref and the visible label of the option")
		}
	case "wait_for":
		if strings.TrimSpace(a.Text) == "" {
			return c, errors.New("wait_for needs the text to wait for")
		}
	case "viewport":
		size := strings.ToLower(strings.TrimSpace(a.Text))
		if size != "mobile" && size != "tablet" && size != "desktop" {
			return c, errors.New("viewport takes mobile, tablet or desktop")
		}
		c.Text = size
	case "scroll", "wait":
		if a.MS <= 0 && do == "wait" {
			c.Timeout = 500
		}
	case "":
		return c, errors.New("each action needs a \"do\"")
	default:
		return c, fmt.Errorf("unknown action %q", do)
	}
	return c, nil
}

func (m *Module) nextID() string {
	return "c" + strconv.FormatUint(m.seq.Add(1), 36)
}

// summarize gives the verdict in one line, so the question that matters most —
// did my app render or not — does not require reading the whole payload.
func summarize(s Snapshot) string {
	switch {
	case len(s.Errors) > 0 && s.Blank:
		return "Blank page — the app crashed on render: " + s.Errors[0].Message
	case s.Blank:
		return "The page rendered nothing (blank)."
	case len(s.Errors) > 0:
		return fmt.Sprintf("Rendered, but hit %d runtime error(s): %s", len(s.Errors), s.Errors[0].Message)
	case !s.Ready:
		return "Still loading."
	default:
		return "Rendered fine at " + s.URL
	}
}
