package runtime

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func fakeJWT(payloadJSON string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payloadJSON)) + ".s"
}

func TestDecodeJWTClaims(t *testing.T) {
	jwt := fakeJWT(`{"sub":"u1","name":"Paul","region":"EU-West","roles":["admin","billing"]}`)
	c := decodeJWTClaims(jwt)
	if c["name"] != "Paul" || c["region"] != "EU-West" {
		t.Fatalf("claims not decoded: %+v", c)
	}
	if decodeJWTClaims("") != nil || decodeJWTClaims("notajwt") != nil {
		t.Errorf("malformed token must decode to nil")
	}
}

func TestContextSectionsText_EndToEnd(t *testing.T) {
	jwt := fakeJWT(`{"sub":"u1","name":"Paul","region":"EU-West","locale":"fr-FR","roles":["admin"]}`)
	app := &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: "app1", Enabled: true},
		Definition: &schema.AppDefinition{
			App: schema.AppMeta{AppID: "app1", Name: "Demo", Version: "1.0"},
			Context: &schema.ContextBlock{Sections: []schema.ContextSection{
				{ID: "user", Builtin: "user", Priority: 1},
				{ID: "greeting", Title: "Context", Template: "User {{user.name}} ({{user.region}}) in {{app.name}} on {{date}}.", Priority: 2},
				{ID: "eu_only", Text: "GDPR applies.", When: "user.region", Priority: 3},
			}},
		},
	}
	agent := &schema.Agent{ID: "main", Role: "coordinator", Context: &schema.ContextBlock{
		Sections: []schema.ContextSection{{ID: "policy", Text: "Be concise.", Priority: 4}},
	}}
	snap := sessionstore.SessionSnapshot{Goal: "ship it", ActiveMode: "build", TurnCount: 2}
	in := TurnInput{UserID: "u1", UserJWT: jwt}

	out := (&Engine{}).contextSectionsText(in, agent, app, snap)
	for _, w := range []string{"Paul", "EU-West", "fr-FR", "Demo", "GDPR applies.", "Be concise."} {
		if !strings.Contains(out, w) {
			t.Errorf("rendered context missing %q:\n%s", w, out)
		}
	}
	// Order: app sections (priority 1,2,3) then agent (4).
	if strings.Index(out, "Be concise.") < strings.Index(out, "GDPR applies.") {
		t.Errorf("agent section must come after app sections:\n%s", out)
	}
}

func TestContextSectionsText_NoBlockEmpty(t *testing.T) {
	app := &appmgr.RuntimeApp{Definition: &schema.AppDefinition{App: schema.AppMeta{AppID: "a"}}}
	out := (&Engine{}).contextSectionsText(TurnInput{UserID: "u"}, &schema.Agent{ID: "main"}, app, sessionstore.SessionSnapshot{})
	if out != "" {
		t.Errorf("no context block → empty, got %q", out)
	}
}

func TestContextSectionsText_WhenGateDropsWhenAbsent(t *testing.T) {
	// No region claim → the GDPR section (when: user.region) is dropped.
	jwt := fakeJWT(`{"sub":"u1","name":"Sam"}`)
	app := &appmgr.RuntimeApp{Definition: &schema.AppDefinition{
		App: schema.AppMeta{AppID: "a", Name: "App"},
		Context: &schema.ContextBlock{Sections: []schema.ContextSection{
			{ID: "eu_only", Text: "GDPR applies.", When: "user.region"},
			{ID: "user", Builtin: "user"},
		}},
	}}
	out := (&Engine{}).contextSectionsText(TurnInput{UserID: "u1", UserJWT: jwt}, &schema.Agent{ID: "m"}, app, sessionstore.SessionSnapshot{})
	if strings.Contains(out, "GDPR") {
		t.Errorf("when-absent section must drop:\n%s", out)
	}
	if !strings.Contains(out, "Sam") {
		t.Errorf("user section should still render:\n%s", out)
	}
}
