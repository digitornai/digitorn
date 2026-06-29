# Browser Demo - Task Examples

## Quick Test

```bash
# Start the daemon with the browser demo config
./digitornd -config examples/browser-demo/app.yaml run
```

## Example Tasks

### 1. Navigate and Screenshot

```bash
./digitorn task "Navigue vers https://example.com et prends un screenshot"
```

### 2. Extract Content

```bash
./digitorn task "Va sur https://news.ycombinator.com et extrais les 5 premiers titres"
```

### 3. Fill a Search Form

```bash
./digitorn task "Navigue vers https://www.google.com, recherche 'digitorn ai agent', et prends un screenshot des résultats"
```

### 4. Multi-step Workflow

```bash
./digitorn task "Va sur https://httpbin.org/forms/post, remplis le formulaire (pizza, taille large, suppliments olives), et soumets le formulaire"
```

## Available MCP Tools

| Tool | Description |
|------|-------------|
| `browser_action` | navigate, click, fill, select, wait |
| `browser_screenshot` | Capture d'écran |
| `browser_extract` | Extraire contenu (markdown, DOM, ASCII) |
| `browser_form` | Remplir/soumettre formulaires |
| `browser_query` | Logs console, infos éléments |

## Configuration

Le serveur MCP `mcp-browser` est configuré dans `app.yaml` :

```yaml
tools:
  modules:
    mcp:
      config:
        servers:
          browser:
            transport: stdio
            command: mcp-browser
            args: ["mcp"]
            timeout: 30
            sandbox:
              permissions: [process.exec, net.http]
```

## Notes

- Le serveur MCP tourne en stdio, Digitorn le lance automatiquement
- Les outils MCP deviennent des outils natifs pour l'agent
- Pas besoin de modifier le code de Digitorn
- Fonctionne avec n'importe quel LLM supporté (Anthropic, OpenAI, etc.)
