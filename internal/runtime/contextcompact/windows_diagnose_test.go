package contextcompact

import (
	"fmt"
	"testing"
)

// TestDiagnose_RealGatewayModels prints what ContextWindowFor returns for the
// actual models the gateway lists. Used to identify why the CLI footer shows
// 65.5k when running a model that has a much larger documented window.
func TestDiagnose_RealGatewayModels(t *testing.T) {
	for _, m := range []string{
		"mimo-v2.5",
		"moonshotai/Kimi-K2.6",
		"moonshotai/kimi-k2.6",
		"deepseek-ai/DeepSeek-V4-Pro",
		"claude-haiku-4-5-20251001",
		"gpt-5-mini",
	} {
		w := ContextWindowFor("openai", m)
		fmt.Printf("  ContextWindowFor(openai, %-32s) = %d (%.1fk)\n", m, w, float64(w)/1024)
	}
}
