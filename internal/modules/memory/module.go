// Package memory is a doc-only placeholder — memory is NOT a bus module.
//
// Unlike a domain module (filesystem, shell : stateless request/response over
// the service bus), working memory mutates SESSION STATE via durable events and
// re-injects into the prompt every turn. It is therefore a RUNTIME SUBSYSTEM,
// not a bus module, and lives in :
//
//   - internal/runtime/memory.go                  — MemoryWriter (set_goal / remember / task_create / task_update)
//   - internal/runtime/context/meta/memory.go     — the meta-tool handlers
//   - internal/runtime/context/prompt/memory.go   — working-memory snapshot injection
//   - internal/runtime/sessionstore               — EventGoalSet / EventMemoryFactAdded / EventTodo* projection
//
// The documented YAML activation contract (docs-site/docs/reference/modules/
// memory.md — "gated by tools.modules.memory") : the module is activated like
// any other, by DECLARING it (presence = enabled). Its 4 LLM-exposed actions
// (memory.set_goal / memory.remember / memory.task_create / memory.task_update)
// are then offered, always-direct :
//
//	tools:
//	  modules:
//	    memory:
//	      config:            # all optional
//	        max_facts: 50
//	        max_todos: 20
//	        redact_secrets: true
package memory
