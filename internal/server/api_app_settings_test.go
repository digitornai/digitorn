package server

import (
	"encoding/json"
	"testing"

	dbmod "github.com/digitornai/digitorn/internal/modules/database"
	"github.com/digitornai/digitorn/pkg/module"
)

// The database module's generated schema must mark the DSN as a password so the
// endpoint redacts it; non-secret fields must pass through untouched.
func TestRedactSecrets_DatabaseConfig(t *testing.T) {
	schema := module.SchemaFromType(&dbmod.Config{})
	if len(schema) == 0 {
		t.Fatal("empty schema")
	}

	value := map[string]any{
		"allow_raw_dsn": true,
		"databases": []any{
			map[string]any{
				"name": "prod",
				"kind": "postgres",
				"dsn":  "postgres://u:secret@h:5432/db",
			},
		},
	}

	out := redactSecrets(deepCopyJSON(value), schema)
	b, _ := json.Marshal(out)
	got := string(b)

	if want := secretSentinel; !contains(got, want) {
		t.Fatalf("dsn not redacted: %s", got)
	}
	if contains(got, "secret@h") {
		t.Fatalf("plaintext DSN leaked: %s", got)
	}
	if !contains(got, `"name":"prod"`) || !contains(got, `"allow_raw_dsn":true`) {
		t.Fatalf("non-secret fields altered: %s", got)
	}

	// The original value must NOT be mutated (deep copy).
	if dbs := value["databases"].([]any); dbs[0].(map[string]any)["dsn"] != "postgres://u:secret@h:5432/db" {
		t.Fatal("original value was mutated")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
