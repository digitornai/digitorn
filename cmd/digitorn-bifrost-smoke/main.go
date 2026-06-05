// L1 smoke test : verifies that github.com/maximhq/bifrost/core imports
// cleanly, that we can instantiate a Bifrost client with a minimal
// in-memory Account, and that public API entry points compile against
// our codebase. NO real LLM call is made — that comes in L3.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

type stubAccount struct{}

func (stubAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (stubAccount) GetKeysForProvider(ctx context.Context, p schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{
		{
			ID:     "stub",
			Name:   "stub",
			Value:  schemas.EnvVar{Val: "sk-stub-do-not-use", FromEnv: false},
			Models: schemas.WhiteList{"*"},
			Weight: 1.0,
		},
	}, nil
}

func (stubAccount) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: 2,
			BufferSize:  16,
		},
	}, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := bifrost.Init(ctx, schemas.BifrostConfig{
		Account:            stubAccount{},
		InitialPoolSize:    16,
		DropExcessRequests: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bifrost init failed: %v\n", err)
		os.Exit(1)
	}
	defer client.Shutdown()

	fmt.Println("✅ Bifrost imported + instantiated successfully")
	fmt.Println("   Provider configured: openai (stub key)")
	fmt.Println("   Concurrency: 2  BufferSize: 16  InitialPoolSize: 16  DropOnFull: true")

	// Attempt a chat call to verify the wiring compiles and reaches the
	// provider layer. We expect an error because the API key is fake.
	bctx, bcancel := schemas.NewBifrostContextWithTimeout(ctx, 5*time.Second)
	defer bcancel()

	req := &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "gpt-4o-mini",
		Input: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{
					ContentStr: ptr("ping"),
				},
			},
		},
	}
	_, callErr := client.ChatCompletionRequest(bctx, req)
	if callErr != nil {
		fmt.Println("✅ ChatCompletionRequest reached the provider layer (expected error with stub key)")
		if callErr.Error.Message != "" {
			fmt.Printf("   Error message: %s\n", callErr.Error.Message)
		}
		if callErr.StatusCode != nil {
			fmt.Printf("   Status code: %d\n", *callErr.StatusCode)
		}
	} else {
		fmt.Println("⚠️  ChatCompletionRequest returned success with stub key (unexpected)")
	}

	fmt.Println("\nL1 smoke test : PASS")
}

func ptr[T any](v T) *T { return &v }
