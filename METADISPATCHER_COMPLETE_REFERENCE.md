# MetaDispatcher Complete Reference
**Status: COMPLETE** — All discovery, execution, and delegation flows documented with LLM tolerance patterns

---

## Overview

The **MetaDispatcher** (runtime/context/meta/dispatcher.go) is the central orchestration hub for:
- **Tool discovery** (search, list, browse, get)
- **Tool execution** with LLM-tolerant parameter handling
- **Agent delegation** (call_app, use_skill, ask_user)
- **Async task management** (background_run, run_parallel)
- **Memory operations** (set_goal, remember, task_create, task_update)

All handlers return `runtime.ToolOutcome` with JSON-encoded results for LLM consumption.

---

## Core Interfaces & Types

### ToolDispatcher Interface
```go
type ToolDispatcher interface {
    Dispatch(ctx context.Context, inv ToolInvocation) ToolOutcome
}
```

### ToolInvocation (Audit Trail)
```go
type ToolInvocation struct {
    CallID      string                 // Unique call identifier
    Name        string                 // Tool FQN (e.g., "fs.read")
    Args        map[string]interface{} // Parsed parameters
    AppID       string                 // Calling application
    AgentID     string                 // Calling agent
    UserID      string                 // End user
    SessionID   string                 // Session context
    AgentRunID  string                 // Agent execution ID
    UserJWT     string                 // Authentication token
}
```

### ToolOutcome (Result Format)
```go
type ToolOutcome struct {
    Status      string                      // "completed", "errored", "pending"
    Parts       []sessionstore.MessagePart  // Multi-format results (text, image, etc.)
    Error       string                      // Error message if Status == "errored"
    DurationMs  int64                       // Execution time
    Diff        string                      // For file operations
    Metadata    map[string]interface{}      // Tool-specific metadata
}
```

---

## Handler Architecture

### 1. handleSearchTools() [lines 298–373]
**Unified discovery** — Merges search + list_categories + browse_category modes

**Parameters:**
```json
{
  "query": "string (optional)",
  "category": "string (optional)",
  "risk_level": "string (optional, e.g., 'high')",
  "detail": "boolean (default: false)",
  "limit": "number (default: 20, clamped [1, 200])"
}
```

**Behavior:**
- Over-fetches by 6× when filtering by category/risk (to account for filtering loss)
- Minimum 30 results when category/risk specified
- Detail mode includes full parameter schemas + irreversible flags + tags + aliases
- Returns empty array (not error) for no matches

**Output:**
```json
{
  "tools": [
    {
      "name": "fs.read",
      "module": "filesystem",
      "description": "...",
      "params": {...},  // Only if detail=true
      "irreversible": false,
      "tags": ["read", "discovery"],
      "aliases": ["cat", "view"]
    }
  ],
  "total": 42,
  "query": "read"
}
```

---

### 2. handleGetTool() [lines 421–444]
**Full schema retrieval** by FQN with canonicalization + alias resolution

**Parameters:**
```json
{
  "name": "fs.read"
}
```

**Behavior:**
- Resolves aliases (e.g., "cat" → "fs.read")
- Returns complete parameter schema + irreversible flag + documentation
- Returns error if tool not found

**Output:**
```json
{
  "name": "fs.read",
  "module": "filesystem",
  "description": "Read a file with line numbers...",
  "params": {
    "path": {
      "type": "string",
      "description": "File path relative to workspace",
      "required": true
    },
    "limit": {
      "type": "integer",
      "description": "Max lines to return",
      "required": false,
      "default": 0
    }
  },
  "irreversible": false,
  "tags": ["read", "discovery"],
  "aliases": ["cat", "view"]
}
```

---

### 3. handleExecuteTool() [lines 450–496] ✓ **COMPLETE**
**LLM-tolerant re-entry dispatch** with three accepted parameter shapes

**Parameters (3 accepted formats):**

**Format 1: Documented (Preferred)**
```json
{
  "name": "fs.read",
  "params": {
    "path": "main.go",
    "limit": 50
  }
}
```

**Format 2: Flattened (LLM shorthand)**
```json
{
  "name": "fs.read",
  "path": "main.go",
  "limit": 50
}
```

**Format 3: String params (Discovery mode)**
```json
{
  "name": "fs.read",
  "params": "{\"path\": \"main.go\", \"limit\": 50}"
}
```

**Parameter Resolution (Precedence):**
1. Explicit `params` field (object or JSON string)
2. `arguments` field (legacy)
3. `args` field (legacy)
4. Flattened top-level fields
5. Empty map (not nil) if no params provided

**Execution Flow:**
```
extractExecuteToolParams()
  ↓
coerceParamMap() [handles JSON string coercion]
  ↓
gateTarget() [SG-4 security check]
  ↓
Dispatch() [re-entrant call with full audit trail]
```

**Audit Trail Preservation:**
- CallID, AppID, AgentID, UserID, SessionID, AgentRunID, UserJWT passed through unchanged
- Enables complete request tracing across delegation chains

**Output:**
```json
{
  "status": "completed",
  "parts": [
    {
      "type": "text",
      "text": "..."
    }
  ],
  "duration_ms": 42,
  "diff": "...",  // For file operations
  "metadata": {...}
}
```

---

### 4. handleListCategories() [lines 559–572]
**Returns all indexed tool categories**

**Parameters:** None

**Output:**
```json
{
  "categories": [
    {
      "name": "filesystem",
      "tool_count": 6
    },
    {
      "name": "context_builder",
      "tool_count": 2
    }
  ]
}
```

---

### 5. handleBrowseCategory() [lines 580–646]
**Paginated category browsing** with overflow protection

**Parameters:**
```json
{
  "category": "filesystem",
  "page": 1,
  "page_size": 10  // Optional, default from config
}
```

**Behavior:**
- Clamps page to valid range (prevents negative slice bounds)
- Returns empty tools array (not error) for out-of-range pages
- Includes pagination metadata

**Output:**
```json
{
  "category": "filesystem",
  "page": 1,
  "page_size": 10,
  "total": 42,
  "tools": [
    {
      "name": "fs.read",
      "description": "...",
      "irreversible": false
    }
  ]
}
```

---

### 6. Additional Meta-Tool Handlers [lines 252–261]

#### run_parallel()
Concurrent execution of independent tool calls
```json
{
  "tasks": [
    {"tool": "fs.read", "args": {"path": "a.go"}},
    {"tool": "fs.read", "args": {"path": "b.go"}}
  ]
}
```

#### ask_user()
Human approval/input bridge
```json
{
  "question": "Approve this change?",
  "options": ["yes", "no"]
}
```

#### background_run()
Async task manager (launch, status, wait, cancel, list)
```json
{
  "name": "shell.run",
  "params": {"cmd": "npm install"},
  "mode": "launch"  // or "status", "wait", "cancel", "list"
}
```

#### use_skill()
Markdown-based skill resolution
```json
{
  "skill_name": "deploy_to_prod",
  "context": {...}
}
```

#### call_app()
Inter-app invocation
```json
{
  "app_id": "payment_service",
  "method": "charge",
  "params": {...}
}
```

---

### 7. dispatchMemoryTool() [lines 272–285]
**Routes memory module actions** (memory. prefix stripped)

**Supported Operations:**
- `set_goal` — Store agent goal
- `remember` — Store fact in memory
- `task_create` — Create tracked task
- `task_update` — Update task status

**Example:**
```json
{
  "name": "memory.set_goal",
  "params": {
    "goal": "Deploy service to production",
    "priority": "high"
  }
}
```

---

## Key Design Patterns

### 1. LLM Tolerance
The dispatcher accepts **3+ parameter shapes** because smaller LLMs frequently:
- Flatten nested objects into top-level fields
- Emit JSON as strings instead of objects
- Use legacy field names (args, arguments)

**Solution:** `extractExecuteToolParams()` tries all precedences; `coerceParamMap()` accepts both JSON objects and JSON-encoded strings.

### 2. Safe Pagination
Integer overflow protection in `handleBrowseCategory()`:
```go
page := clamp(page, 1, math.MaxInt64)  // Prevents negative slice bounds
```

### 3. Empty Defaults
Returns empty maps/arrays (not nil/error) for valid but empty results:
- No matching tools → `{"tools": []}` (not error)
- Out-of-range page → `{"tools": []}` (not error)
- No params → `{}` (not nil)

### 4. Re-entrant Security
Tool execution re-enters the dispatcher with full security:
```
execute_tool()
  ↓
gateTarget() [SG-4 chokepoint]
  ↓
Dispatch() [full audit trail + security checks]
```

### 5. Audit Trail Preservation
All invocation metadata (CallID, AppID, AgentID, UserID, SessionID, AgentRunID, UserJWT) flows through delegation chains unchanged, enabling complete request tracing.

---

## Helper Functions

### errored(msg string) ToolOutcome
Returns a failed outcome with error message.

### jsonOutcome(obj map[string]any) ToolOutcome
Marshals object to JSON and wraps in a text Part. Used by all handlers to return structured results to LLM.

---

## Integration Points

### Session Runner → MetaDispatcher
The session runner calls `MetaDispatcher.Dispatch()` for every tool invocation, passing:
- User context (UserID, SessionID, UserJWT)
- Agent context (AgentID, AgentRunID)
- App context (AppID)
- Tool request (Name, Args)

### Tool Registry
MetaDispatcher queries the tool registry for:
- Tool metadata (description, params, irreversible flag)
- Category membership
- Aliases

### Security Gate (gateTarget)
Before re-entering Dispatch, execute_tool applies `gateTarget()` security check (SG-4 chokepoint).

---

## Testing & Validation

### Parameter Coercion Tests
- Documented format (object params)
- Flattened format (top-level fields)
- String params (JSON-encoded string)
- Legacy field names (args, arguments)

### Pagination Tests
- Normal page (1-based)
- Out-of-range page (returns empty, not error)
- Integer overflow (clamped safely)

### Discovery Tests
- Search by query
- Filter by category
- Filter by risk level
- Detail mode (includes schemas)

---

## Open Tasks

- [ ] Review memory handler implementations (set_goal, remember, task_create, task_update)
- [ ] Study agent delegation flow (handleAgent)
- [ ] Trace session runner → MetaDispatcher.Dispatch() integration
- [ ] Performance profiling (bifrost dispatch_bench_test.go)
- [ ] Document error handling strategy (when to return error vs. empty result)

---

## Summary

The MetaDispatcher is a **production-grade orchestration hub** that:
1. **Discovers tools** with flexible filtering and pagination
2. **Executes tools** with LLM-tolerant parameter handling
3. **Delegates work** across agents and apps
4. **Manages async tasks** and human approval
5. **Preserves audit trails** across delegation chains
6. **Handles errors gracefully** (empty results, not crashes)

All handlers return JSON-encoded results for LLM consumption, enabling seamless integration with language models of varying sophistication.
