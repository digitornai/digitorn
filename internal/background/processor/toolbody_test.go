package processor

import (
	"strings"
	"testing"

	"github.com/mbathepaul/digitorn/internal/background/daemonclient"
)

func TestToolBody_FileWriteIsReadable(t *testing.T) {
	ap := daemonclient.Approval{
		Kind:      "tool_call",
		ToolName:  "filesystem.write",
		RiskLevel: "medium",
		Reason:    "Writing a file changes the workspace — confirm before it happens.",
		ToolParams: map[string]any{
			"path":    `C:\Users\ASUS\.digitorn\workdirs\dcapprove\3442647483eb4a458e4feffcbe4b9429\tgf-8684534034\bot.py`,
			"content": "import discord\nfrom discord.ext import commands\nimport os\n\nbot = commands.Bot()\n\n@bot.command()\nasync def approve(ctx):\n    pass\n",
		},
	}
	got := toolBody(ap)
	t.Logf("\n----- RENDERED -----\n%s\n--------------------", got)

	if !strings.Contains(got, "📄 `bot.py`") {
		t.Errorf("should show the basename, not the full path: %q", got)
	}
	if strings.Contains(got, `\Users\ASUS`) {
		t.Errorf("must NOT dump the full workdir path: %q", got)
	}
	if !strings.Contains(got, "```python") {
		t.Errorf("content should be a python code block: %q", got)
	}
	if !strings.Contains(got, "import discord\nfrom discord") {
		t.Errorf("content must have REAL newlines, not escaped \\n: %q", got)
	}
	if strings.Contains(got, `\n`) {
		t.Errorf("must NOT contain escaped newlines: %q", got)
	}
}

func TestToolBody_TruncatesLongContent(t *testing.T) {
	long := strings.Repeat("line\n", 100)
	ap := daemonclient.Approval{
		ToolName:   "filesystem.write",
		ToolParams: map[string]any{"path": "big.txt", "content": long},
	}
	got := toolBody(ap)
	if !strings.Contains(got, "lignes)") {
		t.Errorf("a 100-line file must note how many lines were hidden: %q", got)
	}
}

func TestToolBody_ShellCommand(t *testing.T) {
	ap := daemonclient.Approval{
		ToolName:   "bash.run",
		RiskLevel:  "high",
		ToolParams: map[string]any{"command": "rm -rf /tmp/cache && echo done"},
	}
	got := toolBody(ap)
	if !strings.Contains(got, "```bash") || !strings.Contains(got, "rm -rf") {
		t.Errorf("a shell command should be a bash block: %q", got)
	}
}
