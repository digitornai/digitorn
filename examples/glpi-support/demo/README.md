# Demo locale GLPI + Digitorn

**Dossier temporaire** pour une demo sur ton PC. Supprime `examples/glpi-support/demo/` quand tu n’en as plus besoin.

## Prérequis

| Composant | Commande / URL |
|-----------|----------------|
| Docker + compose | `docker compose version` |
| Digitorn daemon | `./bin/digitornd -config ./bin/config.yaml` → `:8000` |
| Digitorn background | `./bin/digitorn-background` → `:8090` |
| App installée | `glpi-support` avec YAML `{{secret.*}}` (reload si besoin) |

## Démarrage rapide (5 commandes)

```bash
cd examples/glpi-support/demo
chmod +x *.sh

./start.sh                    # GLPI → http://localhost:8080
./setup-glpi-api.sh           # API + tokens → demo.env (auto)
./run-demo.sh                 # secrets + ticket + webhook
```

Manuel (si `setup-glpi-api.sh` ne suffit pas) :

```bash
cp demo.env.example demo.env  # puis édite demo.env
# One-time GLPI UI: API + client + user token — voir § ci-dessous
./glpi-session.sh && ./push-secrets.sh
./run-demo.sh
```

Puis dans Digitorn : **`/agents/glpi-support`** → **Approvals** → **Approve** → vérifie le ticket dans GLPI.

---

## GLPI — configuration one-time (~10 min)

1. Ouvre **http://localhost:8080** (premier login souvent `glpi` / `glpi`).
2. **Configuration → Général → API** → activer l’API REST.
3. **Configuration → Clients API** → **Ajouter** → note l’**App-Token** → colle dans `demo.env` :
   ```bash
   GLPI_APP_TOKEN=...
   ```
4. **Ton profil → Préférences → Clés d’accès distantes** → génère un **user token** :
   ```bash
   GLPI_USER_TOKEN=...
   ```
5. Session API :
   ```bash
   ./glpi-session.sh
   ```
6. Secrets Digitorn (UI ou script) :
   ```bash
   ./push-secrets.sh
   ```
   Ou manuellement dans l’UI :
   - **Channels** → `GLPI_WEBHOOK_KEY` = `demo-webhook-secret` (valeur par défaut dans `demo.env`)
   - **Secrets** → `GLPI_URL`, `GLPI_APP_TOKEN`, `GLPI_SESSION_TOKEN`

---

## Scripts

| Script | Rôle |
|--------|------|
| `./setup-glpi-api.sh` | Auto-config GLPI REST API + tokens in `demo.env` |
| `./start.sh` | Lance GLPI Docker (`:8080`) |
| `./stop.sh` | Arrête les conteneurs (garde les volumes) |
| `./glpi-session.sh` | Rafraîchit `GLPI_SESSION_TOKEN` dans `demo.env` |
| `./push-secrets.sh` | Envoie les secrets au daemon Digitorn (JWT optionnel) |
| `./create-ticket.sh` | Crée un ticket dans GLPI, affiche l’`id` |
| `./fire-webhook.sh [id]` | Simule l’événement GLPI → Digitorn |
| `./run-demo.sh` | Enchaîne tout pour la demo |

### Demo manuelle (sans créer le ticket via API)

```bash
# Ticket déjà créé à la main dans GLPI avec id=5 :
./fire-webhook.sh demo.env 5
```

### Payload webhook de référence

Voir `sample-webhook-payload.json` — c’est ce que Digitorn attend sur `POST /hook/glpi`.

---

## Scénario demo devant un client

1. **GLPI** : montre le ticket vide (VPN / réseau).
2. **Terminal** : `./fire-webhook.sh demo.env <id>` (ou `./run-demo.sh`).
3. **Digitorn → Executions** : triage + draft spécialiste (KB réseau).
4. **Digitorn → Approvals** : approve.
5. **GLPI** : refresh → réponse publique + ticket résolu.

---

## Option avancée — webhook natif GLPI 11

Si tu es en GLPI 11+, tu peux configurer un webhook sortant au lieu de `fire-webhook.sh` :

- **Administration → Configuration → Webhooks**
- URL : `http://host.docker.internal:8090/hook/glpi`
- Header : `X-API-Key: demo-webhook-secret`
- Payload JSON avec `id`, `status: "new"`, `name`, `content`, `itilcategories_name`, `users_id`
- Vérifier que l’action auto **`queuedwebhook`** tourne (`/var/glpi/logs/` dans le conteneur)

Le `docker-compose.yml` inclut déjà `extra_hosts: host.docker.internal:host-gateway`.

---

## Nettoyage

```bash
./stop.sh
docker compose down -v   # supprime aussi les volumes MySQL/GLPI
rm -f demo.env
# puis supprime ce dossier demo/ entier
```

---

## Dépannage

| Problème | Piste |
|----------|--------|
| Webhook HTTP 401 | `GLPI_WEBHOOK_KEY` ≠ valeur dans Digitorn Channels |
| Webhook HTTP 000 | `digitorn-background` pas lancé |
| initSession échoue | API pas activée ou mauvais App-Token / user token |
| Write-back échoue après approve | `./glpi-session.sh` (session expirée) ou mauvais `GLPI_URL` |
| push-secrets 401 | Renseigne `DIGITORN_TOKEN` ou saisis les secrets dans l’UI |
