---
id: memory
---

# Cognitive Memory

The `memory` module gives an agent a small
working brain: a goal, a todo list, persistent facts that survive
context compaction, and a per-session/per-user store the runtime
auto-injects into the system prompt.

Every action and behaviour on this page maps to real code; entries
are cited with file + line.

## What the agent sees vs what's stored

The memory layer exposes **4 actions** to the LLM and stores **5
layers** internally. The agent doesn't issue queries for memory -
the runtime renders the relevant pieces into the system prompt at
turn start and the agent simply reads them.

```yaml
tools:
  modules:
    memory:
      config:
        working_memory: true       # goal + todos rendered into prompt
        todo_list: true            # alias of working_memory.todo_list
        episodic: true             # per-session event log
        semantic: true             # facts + entity graph (per-user)
        procedural: true           # learned patterns (per-app)
        auto_remember: false       # passive fact extraction (off by default)
        security:
          redact_secrets: true     # default true; matches key/secret/token/auth/...
        runtime: {}                # proactive injection knobs (advanced)
        limits: {}                 # caps on todos, facts, episodes (advanced)
```

The memory module's config schema uses `extra: allow` so it
tolerates forward-compatible knobs.

## The 4 LLM-exposed actions

| Action | Short alias | What it does |
|--------|-------------|--------------|
| `memory.task_create` | `TaskCreate` | Create a task in the todo list. Surfaces in the dedicated client panel. |
| `memory.task_update` | `TaskUpdate` | Update a task's status. |
| `memory.set_goal` | - | Set or replace the session goal. |
| `memory.remember` | `Remember` | Store a fact that survives context compaction. |

The short aliases above are the names exposed to the LLM.
Anything else (`set_plan`, `update_plan_step`, `add_todo`,
`update_todo`,
`note`, `resolve_note`, `add_fact`, `recall`, `forget`,
`track_entity`, `add_relationship`, `checkpoint`, `cache_content`,
`get_snapshot`, `add_episode`, …) referenced in older docs **does
not exist**. The four actions above are the entire LLM-callable
surface.

### `memory.task_create`

Params (`TaskCreateParams`):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `subject` | string | yes | Brief task title (e.g. "Fix authentication bug"). |
| `description` | string | no (default `""`) | What needs to be done. |

```json
{"name": "TaskCreate",
 "arguments": {"subject": "Refactor src/auth/validate.ts",
               "description": "Split into validate_token + load_user"}}
```

The runtime returns the new task's `taskId`.

### `memory.task_update`

Params (`TaskUpdateParams`):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `taskId` | string | yes | Task id returned by `task_create`. |
| `status` | string | yes | `pending` \| `in_progress` \| `completed` \| `blocked`. |

```json
{"name": "TaskUpdate",
 "arguments": {"taskId": "t1", "status": "in_progress"}}
```

### `memory.set_goal`

Params (`SetGoalParams`):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `goal` | string | yes | The session goal in plain language. |

```json
{"name": "memory.set_goal",
 "arguments": {"goal": "Fix the auth bug in src/auth/validate.ts"}}
```

The current goal is rendered at the top of the MEMORY block in every
subsequent system prompt.

### `memory.remember`

Params (`RememberParams`):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `content` | string | yes | The fact to remember in plain text. |

```json
{"name": "Remember",
 "arguments": {"content": "Test command: pytest tests/ -v"}}
```

Stored in **semantic memory** (per-user, per-app), survives context
compaction, and is rendered back into the system prompt at the next
turn (the agent re-reads it like any other prompt section). Secrets
detected in `content` (values matching the redaction patterns -
key, secret, password, token, auth, credential, private, jwt) are
replaced with `[REDACTED]` before storage.

## Memory layers (internal)

declares the data structures
(`MemoryStore`, `MemoryConfig`, `Note`, `Episode`, `Checkpoint`,
`SemanticMemory`, `CachedContent`, `TodoStatus`).

| Layer | Scope | Lifecycle | Backed by |
|-------|-------|-----------|-----------|
| **Working memory** - `goal`, `todos` | per-session | cleared on session end (`cleanup_session`) | `MemoryStore.working` |
| **Episodic** - session events | per-session | cleared on session end | `MemoryStore.episodic` |
| **Semantic** - facts + entity graph | per-user, per-app | persisted to KV backend | `MemoryStore.semantic` (`SemanticMemory`) |
| **Procedural** - learned patterns | per-app | persisted to KV backend | `MemoryStore.procedures` |
| **Cache** - recent file content | per-session | bounded by `limits` | `MemoryStore.cache` (`CachedContent`) |

The runtime maintains one `MemoryStore` per session, keyed by the
**compound `(user_id, session_id)` tuple** - single-key lookup was
a cross-user leak vector and is fixed.

## Session isolation

 Three guarantees verified by the test suite:

- **Per-session working state** - `goal`, `todos`, `episodes`,
  `cache` are scoped by `user_id::session_id`. Two concurrent
  sessions from the same user (or unrelated users sharing a session
  id prefix) never see each other's todos.
- **Per-user semantic memory** - facts written by user A never
  leak into user B's prompt. Earlier versions shared a single
  `_app_semantic` dict; the current code creates a per-user
  `SemanticMemory` lazily, loaded from the KV backend with a
  user-scoped key.
- **Cleanup on session end** - `cleanup_session`
  clears todos, goal, and episodic events when
  the session closes. Semantic + procedural are NOT cleared; they
  persist for the next session.

## Memory injection into the prompt

`get_prompt_sections` returns up to two prompt
sections - the rendered memory snapshot (priority 5) and the memory
instructions (priority 6) - built by
and
`build_memory_instructions`.

The injected MEMORY block looks like:

```
# MEMORY

## Goal
Fix the authentication bug in src/auth/validate.ts

## Todos (3)
- [in_progress] t1: Trace the failing path
- [pending]     t2: Add unit test
- [pending]     t3: Open PR

## Facts (2)
- Test command: pytest tests/ -v
- Project uses FastAPI + SQLAlchemy + Alembic

## Recent activity
... (episodic events)
```

The agent never has to "query" memory - it just reads the prompt.
Calls to `task_create`, `task_update`, `set_goal`, `remember` mutate
the underlying store; the next turn automatically re-renders the
block.

## Configuration knobs

Top-level keys on `tools.modules.memory.config`
(`MemoryModuleConfig`):

| Key | Type | Default | Effect |
|-----|------|---------|--------|
| `working_memory` | bool | `false` | Render goal + todos in the prompt block. |
| `todo_list` | bool | `false` | Enable the todo-list panel. |
| `checkpoint` | bool | `false` | Periodic self-assessment snapshots (advanced). |
| `episodic` | bool | `false` | Record session events. |
| `semantic` | bool \| dict | `{}` | Enable semantic memory. Pass a dict for fine-grained config (vector backend, graph storage, …). |
| `procedural` | bool | `false` | Enable learned patterns layer. |
| `runtime` | dict | `{}` | Proactive injection / content cache / goal guardian knobs (see `MemoryConfig`). |
| `limits` | dict | `{}` | Caps on number of todos / facts / episodes / cached files. |
| `security` | dict | `{}` | `redact_secrets: bool` (default true), `sensitive_patterns: [str]`. |
| `auto_remember` | bool | `false` | If true, the runtime extracts facts from the conversation passively. Off by default - agents should call `Remember` explicitly. |
| `workspace` | string | `""` | Auto-injected by the daemon. Don't set manually. |

The full set of knobs (vector index dim, graph edge limits, cache
TTL, ...) is on `MemoryConfig`.

## Secret redaction

 The redactor scans the value of every
`os.environ[KEY]` whose name matches the sensitive patterns and
replaces matches with `[REDACTED]` in the text before storage.

Default patterns: `key`, `secret`, `password`, `token`, `auth`,
`credential`, `private`, `jwt`. Extend via:

```yaml
tools:
  modules:
    memory:
      config:
        security:
          redact_secrets: true
          sensitive_patterns: [api_key, slack_webhook]
```

Disable explicitly with `redact_secrets: false`.

## What the agent typically does

Common pattern:

1. **Receive a goal** from the user → call `memory.set_goal`.
2. **Plan** → call `memory.task_create` per step (3-7 tasks).
3. **Execute** → before each step, `memory.task_update(status:
   "in_progress")`; after, `memory.task_update(status: "completed")`.
4. **Persist learnings** → `memory.remember` for facts the next
   session should keep (test commands, file locations, gotchas).

The agent never asks "what's my goal?" or "what tasks do I have?" -
the memory block in the prompt already shows them.

## Persistence

`memory.MemoryStore.persist` and `MemoryStore.restore` write/read
the per-user semantic memory to the daemon's KV backend
(). The keying scheme is
`(app_id, user_id)` - facts persist across sessions for the same
(user, app) pair.

There is no separate user-facing API to read/clear memory today.
Operators can clear the KV store via the database directly.
```

## File-based Persistent Memory

In addition to the cognitive memory module (goals, tasks, facts), Digitorn supports a
**file-based persistent memory** that the agent reads and writes directly as Markdown
files on disk. This is the same mechanism Claude Code uses with `CLAUDE.md` and its
`~/.claude/memory/` directory.

File memory is opt-in via `context.sections` in the app YAML. It requires no extra
module — only the `filesystem` module the agent uses to write files.

### `builtin: memory_index` — zero-config memory

The easiest setup: declare one section with `builtin: memory_index`.

```yaml
context:
  sections:
    - id: project_memory
      builtin: memory_index
      when: session.workdir
      priority: 45
```

The runtime:
1. Scans `{workdir}/.digitorn/memory/` for `.md` files at every turn start.
2. Injects their content into the system prompt inside `<system-reminder>` tags (so
   the agent distinguishes dynamic memory from hardcoded instructions).
3. Always appends the **writing directive** — even on the first turn when the directory
   is empty — so the agent knows immediately how and when to persist knowledge.

`MEMORY.md` (if present) is loaded first as the index; all other `.md` files follow in
alphabetical order.

### File format

All memory files are written in **English**. The format mirrors Claude Code's memory
structure exactly, so both tools can read each other's memory:

```markdown
---
name: no-code-comments
description: "User doesn't want comments added when modifying code"
metadata:
  type: feedback
---

Don't add inline or block comments when editing source files.

**Why:** User explicitly asked to avoid comment lines in all modifications.
**How to apply:** When editing any source file, write no comments. Let the code speak for itself.
```

**Four memory types** (used as the `metadata.type` field and as filename prefix):

| Type | Filename prefix | What to store |
|------|----------------|---------------|
| `user` | `user_*.md` | Preferences, expertise, working style |
| `feedback` | `feedback_*.md` | Corrections and direct guidance from the user |
| `project` | `project_*.md` | Domain facts, architecture decisions, business logic |
| `reference` | `reference_*.md` | Commands, paths, URLs, identifiers, contacts |

**`MEMORY.md` index** (required, keep under 50 lines):

```markdown
# Memory Index

- [No code comments](feedback_no_code_comments.md) — Don't add comments when modifying code
- [Deploy process](reference_deploy.md) — make deploy ENV=prod
```

### `file:` / `files:` / `dir:` — custom paths

For full control over which files are loaded:

```yaml
context:
  sections:
    # Single file (read-only context, no writing directive)
    - id: agents_md
      file: AGENTS.md
      optional: true
      priority: 42

    # Multiple files in one section
    - id: conventions
      files:
        - AGENTS.md
        - DIGITORN.md
        - CLAUDE.md
      optional: true
      priority: 43

    # Entire directory (MEMORY.md loaded first, then alphabetical)
    - id: custom_memory
      dir: .my-memory/
      writable: true        # inject writing directive; agent may write here
      optional: true
      priority: 44

    # Claude Code memory for this project (read-only)
    - id: claude_code_memory
      dir: "{{env.home}}/.claude/projects/{{session.workdir_slug}}/memory"
      optional: true
      priority: 44
```

**`ContextSection` fields:**

| Field | Type | Description |
|-------|------|-------------|
| `file` | string | Single file path (relative to workdir or absolute). |
| `files` | list | Multiple file paths — merged with `file:` in order. |
| `dir` | string | Load all `.md` files from this directory (`MEMORY.md` first). |
| `optional` | bool | Silently skip missing or unreadable files (default `false`). |
| `writable` | bool | Inject the memory writing directive (default `false`). Without this, file sections are read-only context. |
| `builtin` | string | Named built-in: `memory_index`, `datetime`, `user`, `session`, `env`, `code_index`. |
| `text` | string | Verbatim text injected as-is. |
| `template` | string | Text with `{{path}}` placeholders filled from the turn's data bag. |
| `when` | string | Gate: path check (`session.workdir`) or comparison (`session.context_pct >= 60`). |
| `priority` | int | Render order (lower = earlier). |

### `when:` — conditional sections

Sections are dropped when `when:` resolves to empty/false. Supports both a plain path
(truthy check) and comparison expressions:

```yaml
# Show only when context is ≥ 60% full
- id: context_pressure
  template: "Context: {{session.context_pct}}% used."
  when: "session.context_pct >= 60"

# Show only when workdir is set
- id: memory
  builtin: memory_index
  when: session.workdir
```

Supported operators: `>` `>=` `<` `<=` `==` `!=`. Both sides are compared as numbers
when both parse as numbers; otherwise compared as strings.

### Template variables

All `template:`, `file:`, `files:`, `dir:`, and `when:` fields support `{{path}}`
placeholders filled from the turn's data bag:

**`session.*`**

| Variable | Value |
|----------|-------|
| `{{session.workdir}}` | `/home/paul/codes/myapp` |
| `{{session.workdir_slug}}` | `-home-paul-codes-myapp` (Claude Code convention) |
| `{{session.turn}}` | Turn number in this session |
| `{{session.goal}}` | Current agent goal |
| `{{session.mode}}` | Active composer mode |
| `{{session.context_pct}}` | Context window used (%) |
| `{{session.context_max}}` | Context window size (tokens) |
| `{{session.tokens}}` | Current context token count |
| `{{session.cost_usd}}` | Session cost so far |
| `{{session.id}}` | Session ID |

**`env.*`**

| Variable | Value |
|----------|-------|
| `{{env.home}}` | User home directory |
| `{{env.os}}` | `linux` / `darwin` / `windows` |
| `{{env.arch}}` | `amd64` / `arm64` |
| `{{env.shell}}` | `bash` / `powershell` |
| `{{env.config_home}}` | `~/.config` (or `$XDG_CONFIG_HOME`) |
| `{{env.tmp}}` | Temp directory |

Also available: `{{user.*}}` (JWT claims), `{{app.*}}`, `{{date}}`, `{{time}}`, `{{datetime}}`.

### Writing directive

When `builtin: memory_index` is declared, or when `writable: true` is set on a
`file:`/`dir:` section, the runtime automatically injects a `<digitorn-directive>` into
the system prompt instructing the agent:

- **When to create** a file (user preferences, constraints, feedback, references)
- **When NOT to write** (ephemeral task state, obvious context)
- **When to update** an existing file (corrections, evolved rules)
- **When to delete** a file (user asks to forget, obsolete facts)
- The exact file format with frontmatter
- How to maintain `MEMORY.md`

The directive is always injected, even on the first turn before any files exist, so the
agent starts building memory from turn one.

### Comparison with cognitive memory

| | Cognitive memory (`memory` module) | File memory (`context.sections`) |
|---|---|---|
| Storage | Daemon KV store | Markdown files on disk |
| Scope | Per-user, per-session | Per-workdir (shared across users) |
| Agent access | Via `memory.*` tools | Via `filesystem.write` |
| Survives restarts | Yes | Yes |
| Human-readable | No (internal) | Yes |
| Requires module | `memory` | `filesystem` only |
| Best for | Tasks, goals, in-session facts | Project conventions, user profile, references |

Both systems are complementary. Most coding apps use both.

## Cross-references

- Built-in tools index (memory aliases):
  [Built-in Tools](04b-builtin-tools.md#memory-tools-gated-by-toolsmodulesmemory)
- Context window management (compaction trigger, summary brain):
  [Context Management](06-context-management.md)
- Config block reference:
  [App Configuration → tools.modules](02-app-config.md#toolsmodules---module-configuration)
- Per-module reference (storage, advanced knobs):
  [modules/reference/memory.md](../reference/modules/memory.md)
