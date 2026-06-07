# Analyse Complète du Projet OpenCode

> Analyse générée le 17 juillet 2025 — 4 sous-agents d'exploration, ~40 minutes d'analyse.

---

## 1. Identité du Projet

| Propriété | Valeur |
|---|---|
| **Nom** | OpenCode |
| **But** | Agent de codage IA en terminal — clone open-source de Claude Code |
| **Langage** | TypeScript (100%) |
| **Framework principal** | Effect-TS (effets, services, runtime fonctionnel) |
| **UI** | Terminal (Ink + React) + Serveur HTTP |
| **LLMs supportés** | Anthropic Claude, OpenAI GPT, Google Gemini, DeepSeek |

---

## 2. Architecture Globale

```
┌─────────────────────────────────────────────────────────────┐
│                       MAIN (src/main.ts)                     │
│                     Effet Runtime Boot                       │
└──────────────────────┬──────────────────────────────────────┘
                       │
       ┌───────────────┼───────────────┐
       │               │               │
┌──────▼──────┐ ┌──────▼──────┐ ┌──────▼──────┐
│   CLI/TUI   │ │   HTTP API  │ │   ACP/MCP   │
│  (Ink+React)│ │ (Effect     │ │  Protocoles │
│             │ │  HttpApi)   │ │  externes   │
└──────┬──────┘ └──────┬──────┘ └─────────────┘
       │               │
       └───────┬───────┘
               │
       ┌───────▼──────────────────────────────────────┐
       │            SESSION ROUTER                     │
       │       (src/session/router.ts)                 │
       └───────────────────┬──────────────────────────┘
                           │
       ┌───────────────────▼──────────────────────────┐
       │         SESSION PROCESSOR                     │
       │  (processor.ts — runLoop LLM + outils)        │
       └───────┬───────────────────┬──────────────────┘
               │                   │
       ┌───────▼──────┐   ┌───────▼──────┐
       │   LLM Layer  │   │  Tool System │
       │ (ai-sdk/     │   │  (18 outils) │
       │  native)     │   │              │
       └──────────────┘   └──────────────┘
```

---

## 3. Système de Sessions & Prompts

### 3.1 Cycle de vie d'une session

1. **Création** — `session.ts` : une session est créée avec un ID unique, un titre, un directory de travail, et un agent associé. Stockée dans SQLite via Drizzle ORM.
2. **Prompt utilisateur** — Le message entre dans le `router.ts` qui dispatch vers `processor.ts`.
3. **Boucle runLoop** (`processor.ts`) :
   - Crée un `AssistantMessage` persisté
   - Appelle `LLM.stream()` (AI SDK ou native)
   - Traite chaque événement du stream :
     - `reasoning-start/delta/end` → accumulation de blocs de raisonnement
     - `tool-input-start/delta/end` → création d'appels d'outils
     - `tool-call` → exécution d'outil avec vérification de permissions
     - `tool-result` → normalisation (images redimensionnées)
     - `text-start/delta/end` → streaming texte
     - `step-finish` → snapshot diff, tracking usage, vérification overflow
   - Retourne `continue`, `stop`, ou `compact`
4. **Multi-step** : si le LLM demande plus d'étapes, la boucle externe relance.
5. **Compaction** : quand le contexte dépasse le seuil, compaction automatique (sélection tête/queue + résumé).
6. **Fork/Revert** : possibilité de forker à un message ou revenir en arrière.

### 3.2 Templates de Prompts

Les prompts sont chargés dynamiquement selon l'agent, la variante, et le provider :

| Template | Usage |
|---|---|
| `codex.txt` | Prompt principal pour Anthropic Claude — inspiré de Claude Code |
| `kimi.txt` | Prompt généraliste, verbeux, pour GPT/autres |
| `trinity.txt` | Prompt concis, pour réponses rapides |
| `plan.txt` | Mode planification — lecture seule, sans outils dangereux |
| `beast.txt` | Mode avancé (plus d'outils) |
| `deepseek.txt` | Pour DeepSeek (modèle chinois) |

Chaque template décrit : l'identité de l'agent, ses règles, les outils disponibles, le format de réponse attendu.

### 3.3 Agents Natifs

| Agent | Rôle |
|---|---|
| `build` | Exécution de commandes shell sécurisées |
| `plan` | Planification en lecture seule |
| `general` | Assistant général |
| `explore` | Exploration et recherche dans le code |
| `scout` | (Expérimental) Agent de surveillance |
| `compaction` | Résumé et compaction de contexte |
| `title` | Génération de titres de sessions |
| `summary` | Génération de résumés de sessions |

### 3.4 Projections

Le système de projections (`projectors.ts`) permet de transformer la liste des messages avant envoi au LLM : filtrage, reformatage, préparation du contexte.

---

## 4. Système d'Outils

### 4.1 Architecture

```
┌───────────────────────────────┐
│       tool-index.ts           │ ← Registre central
│   (collection de tous les     │
│    outils)                    │
└───────────┬───────────────────┘
            │
   ┌────────┼────────┬──────────┐
   │        │        │          │
┌──▼───┐ ┌──▼───┐ ┌──▼───┐ ┌──▼───┐
│tool. │ │schema│ │trunc.│ │json- │
│ts    │ │.ts   │ │ate.ts│ │schema│
│define│ │ToolID│ │output│ │.ts   │
│()    │ │      │ │()    │ │      │
│ask() │ │      │ │      │ │      │
└──────┘ └──────┘ └──────┘ └──────┘
```

### 4.2 Les 18 Outils

| Outil | Fichier .ts | Description |
|---|---|---|
| **read** | `read.ts` | Lecture fichiers avec numéros de ligne, images, PDF, outline |
| **write** | `write.ts` | Écriture/remplacement de fichiers |
| **edit** | `edit.ts` | Éditions chirurgicales (ligne, texte, insert_after, etc.) |
| **glob** | `glob.ts` | Recherche par motif glob, triés par date |
| **grep** | `grep.ts` | Recherche de contenu par regex |
| **shell** | `shell.ts` | Exécution de commandes shell |
| **apply_patch** | `apply_patch.ts` | Application de patches diff structurés |
| **lsp** | `lsp.ts` | Opérations LSP (définition, références, hover, symboles) |
| **websearch** | `websearch.ts` | Recherche web (Exa/Parallel) |
| **webfetch** | `webfetch.ts` | Récupération d'URL (HTML→Markdown) |
| **task** | `task.ts` | Délégation à sous-agent spécialisé |
| **skill** | `skill.ts` | Chargement de compétence |
| **todo** | `todo.ts` | Mise à jour de liste de tâches |
| **question** | `question.ts` | Interaction utilisateur (choix, formulaire) |
| **plan** | `plan.ts` | Validation de plan |
| **repo_clone** | `repo_clone.ts` | Clonage de dépôt git |
| **repo_overview** | `repo_overview.ts` | Résumé de structure de dépôt |
| **plan_enter** | (description .txt) | Passage en mode planification |

### 4.3 Mécanismes Clés

- **Permissions** : chaque outil critique appelle `ctx.ask()` pour demander autorisation
- **Descriptions .txt** : chaque outil a une description lisible par le LLM chargée par import statique
- **Troncature** : les sorties > 2000 lignes ou 50KB sont tronquées avec sauvegarde intégrale
- **Registre** : `registry.ts` scrape les outils natifs + charge les outils plugins, avec filtrage par agent et capacités du modèle

---

## 5. Infrastructure Technique

### 5.1 Effect-TS

Le projet utilise Effect-TS comme framework fonctionnel :
- **Services** : tous les composants sont des `Effect.Service` avec injection de dépendances via `Layer`
- **État mutable contrôlé** : `InstanceState` permet de l'état impératif dans un cadre fonctionnel
- **Runtime** : `makeRuntime()` dans `src/effect/run-service.ts` boot le runtime Effect
- **Concurrence structurée** : via `Scope` et `Fiber`

### 5.2 Base de Données

- **SQLite** via Drizzle ORM
- **Schéma** : `src/storage/schema.sql.ts` — tables pour sessions, messages, parts, todos
- **Stockage** : `src/storage/storage.ts` encapsule les opérations CRUD
- **DB** : `src/storage/db.ts` configure la connexion SQLite

### 5.3 Plugins

- **Lifecycle hooks** : `src/plugin/index.ts` — hooks pour tool execution, text completion, system prompt
- **Loader** : `src/plugin/loader.ts` — chargement dynamique des plugins depuis des répertoires
- **Installation** : `src/plugin/install.ts` — gestionnaire d'installation/activation
- **Méta-données** : `src/plugin/meta.ts` — informations sur les plugins

### 5.4 MCP & ACP

- **MCP** (Model Context Protocol) : `src/mcp/` — outils MCP intégrés comme outils natifs
- **ACP** (Agent Client Protocol) : `src/acp/` — session.ts, tool.ts, event.ts, directory.ts — protocole pour clients agents

### 5.5 Providers LLM

- **Abstraction** : `src/provider/provider.ts` — interface commune Provider avec méthodes `stream()`, `generate()`
- **Transformation** : `src/provider/transform.ts` — adaptation des formats de messages
- **Dual-path LLM** :
  - **AI SDK** (`ai-sdk.ts`) : pour la plupart des providers, utilise `streamText()` du SDK `ai`
  - **Native** (`native-runtime.ts` + `native-request.ts`) : pour OpenAI, Anthropic, et providers opencode avec SDK natif

### 5.6 Bus d'Événements

`src/bus/index.ts` — système de bus pour la communication inter-composants (sessions, plugins, UI).

---

## 6. Interface Terminal (TUI)

### 6.1 Stack

- **Ink** (React pour terminal) + **React** + **Ink UI** (composants material)
- **Routage** : système de route custom (home, session, themes, models, sessions list)
- **Thèmes** : support multi-thèmes (clair/sombre/custom)

### 6.2 Structure

```
TUI App
├── Providers: Theme, Route, Toast, Plugin
├── Routes:
│   ├── Home              — page d'accueil avec tips
│   ├── Session           — la session de chat active
│   ├── Sessions          — liste des sessions
│   ├── Themes            — sélection de thème
│   └── Models            — sélection de modèle
├── Composants:
│   ├── Prompt            — zone de saisie
│   ├── Diff Viewer       — visualisation de diffs
│   ├── Notification      — toasts/notifications
│   └── Sidebar           — panneaux latéraux (context, MCP, LSP, todo, files)
└── Feature Plugins (20):
    ├── Sidebar: context, mcp, lsp, todo, files, footer
    ├── Home: footer, tips, tips-view
    ├── Prompt: context (token counter)
    ├── System: notifications, plugins, session-v2, which-key, diff-viewer
    └── Session: index, dialog, preview-pane, util
```

### 6.3 Communication TUI↔Serveur

Le TUI communique avec le serveur HTTP via une **queue async** (`tui-control.ts`) :
- `nextTuiRequest()` / `submitTuiResponse()` — canal de contrôle bidirectionnel
- Événements TUI : `prompt.append`, `command.execute`, `toast.show`, `session.select`

---

## 7. Serveur HTTP

### 7.1 Architecture

Basé sur **Effect HttpApi** (`effect/unstable/httpapi`) :

```
OpenCodeHttpApi
├── RootHttpApi           — APIs racine (ControlApi, GlobalApi)
├── EventApi              — SSE events
└── InstanceHttpApi       — APIs liées à une instance
    ├── ConfigApi, ExperimentalApi, FileApi, InstanceApi
    ├── McpApi, ProjectApi, PtyApi, QuestionApi
    ├── PermissionApi, ProviderApi
    ├── SessionApi         ← API session principale
    ├── SyncApi, V2Api, TuiApi, WorkspaceApi
```

### 7.2 Middleware Chain

1. **SchemaErrorMiddleware** — validation des schémas
2. **Authorization** — Basic Auth (token en query/header)
3. **WorkspaceRoutingMiddleware** — routing local/remote (proxy HTTP)
4. **InstanceContextMiddleware** — fournit le contexte d'instance

### 7.3 API Sessions (26 endpoints)

CRUD complet des sessions + messages + parts + fork + revert + permissions.

### 7.4 API TUI (12 endpoints)

Contrôle à distance de l'interface TUI : prompt, commandes, navigation, notifications.

---

## 8. Points Forts et Dette Technique

### Points Forts

- ✅ **Architecture modulaire** — découpage clair, services Effect-TS bien séparés
- ✅ **Dual-path LLM** — support AI SDK + natif selon le provider
- ✅ **Système de plugins** — extensible par lifecycle hooks
- ✅ **Descriptions .txt** — séparation claire entre code et métadonnées LLM
- ✅ **Gestion du contexte** — compaction automatique, projection, détection d'overflow
- ✅ **Protection anti-boucle** — détection de DOOM_LOOP (3 mêmes appels d'outil consécutifs)
- ✅ **Types forts** — Effect Schema pour validation des paramètres
- ✅ **Troncature intelligente** — sorties longues sauvegardées avec aperçu
- ✅ **Multi-modèle** — 4+ providers supportés avec prompts adaptés
- ✅ **Streaming** — événements LLM streamés en temps réel vers le client

### Dette Technique / Points d'Attention

- ⚠️ **Complexité du processeur** — `processor.ts` fait ~880 lignes, beaucoup de logique monolithique
- ⚠️ **Dual-path LLM** — deux implémentations à maintenir (AI SDK + natif)
- ⚠️ **Templates de prompts** — certains templates (deepseek.txt, gemini.txt) sont marqués comme manquants en commentaire
- ⚠️ **Plusieurs TODO v2** — le code contient des références à une version 2 non finalisée
- ⚠️ **Effect unstable** — dépend sur plusieurs APIs `effect/unstable` (HttpApi, HttpServer)
- ⚠️ **Dépendance externe** — blocage sur provider DeepSeek (erreur `reasoning_content`)
- ⚠️ **Tests** — peu de tests visibles dans la structure des répertoires explorés

---

## 9. Flux Complet : de l'Input Utilisateur à la Réponse

```
Utilisateur tape un message
       │
       ▼
TUI (Ink/React) → POST /session/:id/message → serveur HTTP
       │
       ▼
Router → Session Processor (processor.ts)
       │
       ├── 1. Création AssistantMessage (persisté SQLite)
       ├── 2. Construction du prompt (system + historique + outils)
       ├── 3. LLM.stream() → AI SDK ou Native SDK
       ├── 4. Traitement événements streamés :
       │      ├── reasoning → accumulation, stockage
       │      ├── tool-call → exécution outil (permission check)
       │      ├── tool-result → normalisation, complétion
       │      ├── text → streaming vers le client
       │      └── step-finish → usage tracking, snapshot
       ├── 5. Décision : continue / stop / compact
       └── 6. Retour au client (SSE ou JSON)
```

---

## 10. Statistiques du Code

| Métrique | Valeur |
|---|---|
| **Langage** | TypeScript (100%) |
| **Fichiers .ts** | ~350+ |
| **Lignes de code** | ~60,000+ (estimation) |
| **Outils** | 18 outils + descriptions .txt |
| **Agents natifs** | 8 |
| **Templates de prompt** | 4+ (codex, kimi, trinity, plan, beast) |
| **Plugins TUI** | 20 feature-plugins |
| **Endpoints HTTP** | 80+ |
| **Providers LLM** | 4+ (Anthropic, OpenAI, Google, DeepSeek) |

---

*Analyse réalisée avec 4 sous-agents d'exploration spécialisés, couvrant : sessions/prompts, outils/agents, infrastructure technique, et interface TUI/serveur HTTP.*
