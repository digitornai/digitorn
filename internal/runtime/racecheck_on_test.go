//go:build race

package runtime_test

// raceEnabled is true when the test binary is built with -race. Stress/latency
// assertions are skipped in that mode : the race instrumentation is 5-10x
// slower, so wall-clock bounds are meaningless (the code path still runs, so
// the race detector still inspects it).
const raceEnabled = true
