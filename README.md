# Ol1nBot

[![CI](https://github.com/lioilsources/Ol1nDevBot/actions/workflows/ci.yml/badge.svg)](https://github.com/lioilsources/Ol1nDevBot/actions/workflows/ci.yml)
[![Docker](https://github.com/lioilsources/Ol1nDevBot/actions/workflows/docker.yml/badge.svg)](https://github.com/lioilsources/Ol1nDevBot/actions/workflows/docker.yml)

Discord bot pro server Ol1n: publikuje deploye, nové verze ze storů, metriky, týdenní GitHub changelog
a milníky; kategorie a kanály pro appky zakládá sám podle `apps.yaml`. Plán: [`Prompts/00-DISCORD_BOT_PLAN.md`](Prompts/00-DISCORD_BOT_PLAN.md).

Jeden Go binár (bez CGO), SQLite, Docker Compose na JODA. Bot externí API (Apple, Google, GitHub) jen **čte**
a píše pouze do Discordu.

## Stav implementace

| Fáze | Obsah | Stav |
|---|---|---|
| 0 | skeleton, Gateway (`GUILDS`), `/bot status`, reconcile INFRA | ✅ |
| 1 | deploy hook + HMAC, outbox, `#deploys`, GitHub changelog + LLM, `/changelog now`, per-app kategorie | ✅ |
| 2 | Apple lookup, ASC stavy verzí, Play tracks, thread per verze, `first_release` + oznámení nové appky | ✅ |
| 3 | ASC Sales, Play GCS CSV, `metrics_daily`, denní řádek + týdenní digest, status board, milníky, `/metrics` | ✅ |
| 3+ | ASC Analytics (impressions, page views pro iOS) | ❌ plán odkládá, iOS impressions zatím 0 |
| 4 | recenze, Approve tlačítka, `git commit` z `/app add`, fsnotify, scheduled events, docker events watcher | ❌ |

## Architektura

```
cmd/ol1nbot            main: wiring, cron joby, HTTP server, graceful shutdown
internal/config        env + apps.yaml (+ runtime overrides z /app add), validace
internal/store         SQLite (modernc), golang-migrate, sqlc (query.sql → db/)
internal/events        typované eventy, deterministické dedup klíče, cílové kanály
internal/outbox        Emit → řádek per (event, kanál), INSERT OR IGNORE, subscribers
internal/discord       Formatter (embedy), Publisher (outbox → Discord, retry/backoff),
                       Reconciler (apps.yaml → kanály, archivace), StatusBoard, slash příkazy
internal/sources/*     deployhook, github, apple (lookup + ASC), gplay (tracks + GCS CSV), versions
internal/milestones    deklarativní pravidla + engine
internal/changelog     GitHub aktivita → LLM (LiteLLM) → markdown, fallback bez LLM
internal/metrics       denní řádek, týdenní digest, /metrics
internal/health        last_success_at, 3 chyby po sobě → jedna zpráva do #bot-log, pak "znovu funguje"
internal/scheduler     robfig/cron v Europe/Prague, SkipIfStillRunning
```

Tok: zdroj → `Emit(event)` → tabulka `outbox` (jeden řádek na cílový kanál, dedup klíč `event@kanál`)
→ Publisher (každých 5 s nebo hned po notifikaci) → Discord. Restart nic neztratí, 429 řeší discordgo
(`Retry-After`), ostatní chyby exponenciální backoff 30 s → 1 h, po 12 pokusech `dead`.

### Plán jobů (Europe/Prague)

| Job | Kdy | Podmínka |
|---|---|---|
| `apple-lookup` | každých 30 min | vždy (public API) |
| `asc-versions` | každé 2 h | `ASC_*` |
| `asc-sales` | 08:00 (poslední 3 dny) | `ASC_*` + `ASC_VENDOR_NUMBER` |
| `play-tracks` | každých 30 min | `GPLAY_SA_JSON_PATH` |
| `play-stats` | 08:15 | + `GPLAY_REPORTS_BUCKET` |
| `metrics-daily` | 08:45 | jen pokud jsou data |
| `github-changelog` | po 09:00 | `GITHUB_TOKEN` |
| `metrics-weekly` | po 09:30 | jen pokud jsou data |
| `status-board` | každou hodinu + po verzi/deployi | vždy |
| `reconcile` | start + každých 6 h + `/app sync` | vždy |

Milníky se vyhodnocují po každé nové verzi, deployi a importu metrik.

## Nastavení

Ruční kroky (Discord Developer Portal, ASC klíč, Play service account, GitHub PAT) jsou v plánu, sekce 2.

1. `cp .env.example .env` a vyplnit. Povinné jsou jen `DISCORD_BOT_TOKEN`, `DISCORD_APP_ID`,
   `DISCORD_GUILD_ID`; každá další integrace se bez svých klíčů sama vypne (v logu je proč).
2. `cp apps.yaml.example apps.yaml` a doplnit appky. U appek, které už ve storech jsou, dát
   `launched: true` — první nalezené verze se pak zapíšou potichu a neohlásí se jako nové
   (jinak by bot při prvním startu oznámil všechny appky jako "nové"). Lifetime stažení z doby
   před botem jde doplnit přes `downloads_baseline`, aby seděly milníky `downloads_N`.
3. Na serveru založit roli `ops` (pingne se při failed deployi; název jde změnit v `discord.ops_role`).

### Lokálně (testovací guild "Ol1nBot Dev")

```sh
make test
make run      # načte .env, DB ./ol1nbot.db, apps.yaml
```

### JODA

Image staví GitHub Actions (`.github/workflows/docker.yml`) pro `linux/amd64` i `linux/arm64`
do `ghcr.io/lioilsources/ol1ndevbot`: `latest` z `main`, `X.Y.Z` a `X.Y` z tagů `vX.Y.Z`, plus `sha-<commit>`.

```sh
make docker                         # lokální build (volitelně)
# deploy/docker-compose.yml → do compose stacku, deploy/Caddyfile.snippet → Caddy
```

Image je distroless (nonroot); healthcheck je `/ol1nbot healthcheck` (volá `GET /healthz`).

## Deploy hook

`POST https://hooks.ol1n.com/discordbot/deploy`, hlavička `X-Signature: sha256=<hex HMAC-SHA256 těla>`
(`DEPLOY_HOOK_SECRET`).

```json
{ "service": "storyteller-api", "env": "prod", "version": "v1.4.2", "commit": "abc123",
  "url": "https://storyteller.ol1n.com", "status": "success", "notes": "...",
  "repo": "lioilsources/storyteller-api", "duration": 95 }
```

`repo` (odkaz na commit) a `duration` (sekundy) jsou volitelné. Odpověď `202` = nový deploy, `200` s
`"duplicate": true` = už byl přijat. Ze skriptu: [`deploy/notify-deploy.sh`](deploy/notify-deploy.sh),
z GitHub Actions: [`deploy/github-actions-step.yml`](deploy/github-actions-step.yml).

## Slash příkazy (jen Administrator)

`/app add|remove|sync|list`, `/metrics <slug> [7d|30d]`, `/changelog now [repo]`, `/deploy last <service>`,
`/bot status`. `/app add` otevře formulář, uloží appku do DB (`apps_override`), hned založí kanály
a pošle do `#bot-log` YAML snippet k doplnění do `apps.yaml` v repu.

## Rozhodnutí a odchylky od plánu

- **Oznámení nové appky** jde až s prvním živým vydáním (milník `first_release`), ne už při přidání
  do `apps.yaml` — v tu chvíli ještě nejsou odkazy do storů. Přidání slugu se projeví jen v `#bot-log`
  (reconcile diff).
- **Dedup klíč deploye** obsahuje i `env` a `status` (`deploy:<service>:<env>:<version>:<commit>:<status>`),
  aby se úspěšný redeploy po failu stejné verze neztratil.
- **Kategorie "📱 APPS"** nevzniká — Discord neumí vnořené kategorie; každá appka má vlastní
  kategorii `<emoji> <Název>`.
- **`#announcements`** se zakládá jako News kanál; pokud server není Community, spadne na textový
  kanál (poznamená se v `#bot-log`). Crosspost se zkouší jen tam, kde funguje.
- **Milníky nad historií**: pravidla nad verzemi (`first_release`, `v1_0`, `both_stores`) se spustí
  jen tehdy, když neplatila už pro potichu zapsané verze launched appky.
- **Play store performance**: CSV má "Store listing visitors" (→ page views); impressions v něm
  nejsou. Názvy souborů v bucketu jsou podle dokumentace, ale **je potřeba ověřit** na reálném
  bucketu (parser hledá sloupce tolerantně podle názvu).
- Test Discord REST je in-memory fake implementující stejné rozhraní jako `*discordgo.Session`
  (místo httptest serveru).

## Otevřené otázky (plán, sekce 6)

1. Jak deployuje **olin.now** — `curl` hook, nebo je potřeba `docker events` watcher? (Watcher není
   implementovaný; napojil by se na `deployhook.Handler.Accept`.)
2. Reálný seznam appek (bundle_id / package / ASC app id / repos) do `apps.yaml`.
3. `@everyone` — default: jen nové appky (`everyone_on_new_app: true`, `everyone_on_milestone: false`).
4. `/app add` → zápis do `apps.yaml` přes git commit (Phase 4), zatím DB + snippet.
5. Role per appka (`Manage Roles`) — zatím ne.
6. Přístup service accountu do GCS bucketu s Play reporty.

## Vývoj

- `make generate` po změně `internal/store/query.sql` nebo migrací (sqlc).
- `make golden` po záměrné změně vzhledu embedů (`internal/discord/testdata/*.golden.json`).
