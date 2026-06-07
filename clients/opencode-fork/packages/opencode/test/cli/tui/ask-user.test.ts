import { expect, test } from "bun:test"
import { formFieldToQuestion, encodeQuestionReply } from "../../../src/cli/cmd/tui/context/digitorn"

test("formFieldToQuestion maps each field type to an opencode question", () => {
  const sel = formFieldToQuestion({ name: "fw", label: "Framework", type: "select", options: ["A", "B"], default: "B" })
  expect(sel.multiple).toBe(false)
  expect((sel.options as any[]).map((o) => o.label)).toEqual(["A", "B"])
  expect(sel.value).toBe("B")
  expect(sel.custom).toBe(true) // escape hatch on by default

  const multi = formFieldToQuestion({ name: "feat", type: "multiselect", options: ["X", "Y"] })
  expect(multi.multiple).toBe(true)

  const strict = formFieldToQuestion({ name: "fw", type: "select", options: ["A"], allow_custom: false })
  expect(strict.custom).toBe(false)

  const bool = formFieldToQuestion({ name: "docker", type: "boolean", default: true })
  expect((bool.options as any[]).map((o) => o.label)).toEqual(["Yes", "No"])
  expect(bool.value).toBe("Yes")

  const text = formFieldToQuestion({ name: "name", type: "text", default: "my-app", description: "the app name" })
  expect(text.options).toEqual([])
  expect(text.custom).toBe(true)
  expect(text.value).toBe("my-app")
  expect(text.question).toContain("the app name") // description folded into the prompt
})

test("encodeQuestionReply rebuilds a form JSON object with type coercion", () => {
  const shape = {
    mode: "form" as const,
    fields: [
      { name: "count", type: "integer" },
      { name: "ratio", type: "number" },
      { name: "enabled", type: "boolean" },
      { name: "fw", type: "select" },
      { name: "feat", type: "multiselect" },
    ],
  }
  const answers = [["5"], ["1.5"], ["Yes"], ["FastAPI"], ["Auth", "Tests"]]
  const out = JSON.parse(encodeQuestionReply(shape, answers))
  expect(out).toEqual({ count: 5, ratio: 1.5, enabled: true, fw: "FastAPI", feat: ["Auth", "Tests"] })
})

test("encodeQuestionReply comma-joins multi-select (the daemon splits on comma)", () => {
  expect(encodeQuestionReply({ mode: "multi" }, [["Auth", "Tests"]])).toBe("Auth, Tests")
})

test("encodeQuestionReply passes plain / content text through", () => {
  expect(encodeQuestionReply({ mode: "plain" }, [["PostgreSQL"]])).toBe("PostgreSQL")
  expect(encodeQuestionReply({ mode: "content" }, [["edited markdown"]])).toBe("edited markdown")
  expect(encodeQuestionReply(undefined, [["fallback"]])).toBe("fallback")
})
