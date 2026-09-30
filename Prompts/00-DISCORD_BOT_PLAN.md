# DISCORD_BOT_PLAN.md — "Ol1nBot": Discord bot pro správu komunikace na Ol1n serveru

Handoff dokument pro Claude Code (Opus). Cíl: samostatný Go service běžící v Docker Compose na JODA,
který na Discord serveru Ol1n automaticky publikuje deploye, nové verze/appky ze storů, metriky,
týdenní changelog GitHub repos, milestone announcements a sám zakládá kanály/kategorie pro nové appky.

---

## 0. Kontext a omezení (neměnit bez dotazu)

- Jazyk: Go 1.23+, jeden binár, Docker Compose na JODA (`oli`), Postgres nebo SQLite (preferuj **SQLite** — data jsou malá, jeden proces).
- Migrace `golang-migrate`, dotazy `sqlc`, konfigurace přes env + `apps.yaml`.
- Inbound webhooky přes Cloudflare Tunnel + Caddy: `https://hooks.ol1n.com/discordbot/...`.
- LLM shrnutí (changelog) přes LiteLLM gateway na Sparku (OpenAI-compatible endpoint), model konfigurovatelný env `LLM_MODEL`.
- Žádné autonomní akce mimo Discord. Bot pouze **čte** externí API (Apple, Google, GitHub) a **píše** do Discordu.
- Zdroj pravdy o appkách = `apps.yaml` v repu (ne stav Discordu). Discord je z něj odvozený (idempotentní reconcile).

---

## 1. Co je na Discordu možné (rekapitulace pro návrh)

| Potřeba | Discord mechanismus | Poznámka |
|---|---|---|
| Posílat zprávy do kanálů | Bot token + REST `POST /channels/{id}/messages`, **embeds** | Embed = strukturovaná karta (title, fields, thumbnail, color, timestamp). Ideál pro deploy/release karty. |
| Editovat starou zprávu (living "status board") | `PATCH /channels/{id}/messages/{id}` | Např. pinnutá zpráva "Aktuální verze všech appek", kterou bot přepisuje. |
| Jednoduché posílání bez bota | Discord **Webhook URL** na kanál | Vhodné jako fallback / pro CI (`curl` z GitHub Actions). Ale nemůže zakládat kanály. |
| Zakládat kanály/kategorie, přesouvat, nastavovat práva | Bot s permission `Manage Channels` (+ `Manage Roles` pro role per appka) | Kategorie = kanál typu 4, textový = typ 0, forum = typ 15, announcement = typ 5. |
| Announcement kanál | Typ 5 (News) — zprávy se dají "publishnout" do followujících serverů | Použít pro `#announcements`. |
| Vlákna per release | `POST /channels/{id}/threads` nebo forum posty | Diskuse k jednomu release bez zahlcení kanálu. |
| Slash příkazy (`/app add`, `/metrics`) | Application Commands registrované na guild (okamžité, bez 1h propagace) | Odpověď do 3 s, jinak `defer` + follow-up. |
| Plánované eventy | `POST /guilds/{id}/scheduled-events` | Volitelné (např. "release stream"). |
| Reakce/tlačítka | Message components (buttons, select) | Např. tlačítko "Zobrazit changelog celý" / "Approve announcement". |
| Realtime přijímání | Gateway (WebSocket) | Nutné jen pro slash příkazy + tlačítka. Intents: `GUILDS`. **Message Content intent není potřeba.** |

Limity: 2000 znaků/zpráva, 6000 znaků/embed celkem, 25 fields/embed, 10 embedů/zpráva, rate limit ~5 zpráv/5 s per kanál. Max 500 kanálů na server, 50 kanálů v kategorii.

---

## 2. Jak to nadeklarovat (jednorázový setup — ruční kroky pro Ol1na)

### 2.1 Discord Developer Portal
1. https://discord.com/developers/applications → **New Application** "Ol1nBot".
2. Záložka **Bot** → Reset Token → uložit jako `DISCORD_BOT_TOKEN`. Vypnout "Public Bot".
3. Privileged intents: **žádné** nezapínat.
4. **OAuth2 → URL Generator**: scopes `bot`, `applications.commands`; permissions:
   `View Channels, Manage Channels, Manage Roles (volit.), Send Messages, Send Messages in Threads, Create Public Threads, Embed Links, Attach Files, Read Message History, Add Reactions, Mention Everyone, Manage Messages, Pin Messages`.
   Otevřít vygenerovanou URL, pozvat na server.
5. Zapsat `DISCORD_APP_ID`, `DISCORD_GUILD_ID` (Developer Mode → pravý klik na server → Copy ID).
6. Na serveru přesunout roli bota **nad** role, které bude spravovat.

### 2.2 Apple (App Store Connect API)
1. App Store Connect → Users and Access → **Integrations → App Store Connect API** → Generate API Key, role **Sales and Reports** (stačí pro Sales & Trends + Analytics), stáhnout `.p8`.
2. Env: `ASC_KEY_ID`, `ASC_ISSUER_ID`, `ASC_PRIVATE_KEY_PATH` (mount jako secret).
3. Auth = JWT ES256, platnost ≤ 20 min, `aud: appstoreconnect-v1`.
4. Zdroje:
   - **Nové verze/stavy**: `GET /v1/apps/{id}/appStoreVersions` (stav `READY_FOR_SALE` = live). Alternativa bez auth: `https://itunes.apple.com/lookup?bundleId=…` (public, ~24h zpoždění, ale bez klíče — použít jako **primární** detekci "je to v storu", ASC API pro stav review).
   - **Stažení/impressions**: Analytics Reports API (`analyticsReportRequests` → report `App Store Downloads`, `App Store Discovery and Engagement` — obsahuje Impressions, Product Page Views). Data přichází s ~1–2 dny zpožděním, jako CSV.gz. Pozor: report request je ONGOING, vytvořit jednou per app, pak stahovat instance.
   - Sales & Trends (`salesReports`, `reportType=SALES`, DAILY) — jednodušší, dává units/day. Použít v Phase 1, Analytics v Phase 2.

### 2.3 Google Play
1. Google Cloud projekt → enable **Google Play Android Developer API** + **Google Play Developer Reporting API**.
2. Service account, JSON klíč → `GPLAY_SA_JSON_PATH`. V Play Console → Users and permissions → pozvat SA email, oprávnění "View app information" + "View financial data" (pro statistiky).
3. Zdroje:
   - **Verze v tracku**: `edits.insert` (read-only edit) → `edits.tracks.get(production)` → `releases[].versionCodes`, `name`, `status` → edit **nepotvrzovat, smazat** (`edits.delete`). Jediný spolehlivý způsob přes API.
   - **Instalace/statistiky**: Reporting API `vitals`/`installs` je omezené; **primární zdroj = CSV exporty v GCS bucketu** `pubsite_prod_rev_XXXXXXXX` (Play Console → Download reports → cesta). Report `stats/installs/installs_<pkg>_<YYYYMM>_overview.csv` (daily user installs, uninstalls). Store listing "impressions" jsou v `stats/store_performance/…` (acquisition). SA potřebuje na bucket `Storage Object Viewer` (nastavit v Play Console → Download reports → "Copy Cloud Storage URI" + přidat SA).
   - Google **Play Developer Reporting API** (`crashrate`, `anr`) — bonus do metrik.

### 2.4 GitHub
- Fine-grained PAT (read-only: Contents, Metadata, Pull requests) → `GITHUB_TOKEN`, org/user `lioilsources`.
- Repos k sledování: buď explicitní list v `apps.yaml`, nebo všechny nearchivované repa uživatele (default: explicitní list + přepínač `github.watch_all: true`).

### 2.5 Deploy hook (olin.now a další)
- Bot vystaví `POST https://hooks.ol1n.com/discordbot/deploy` s HMAC-SHA256 hlavičkou `X-Signature` (secret `DEPLOY_HOOK_SECRET`).
- Payload: `{ "service": "storyteller-api", "env": "prod", "version": "v1.4.2", "commit": "abc123", "url": "https://storyteller.ol1n.com", "status": "success|failed", "notes": "..." }`.
- Kdo volá: GitHub Actions step (`curl`), nebo deploy skript na JODA/Sparku. **Otevřená otázka:** jak přesně olin.now deployuje — pokud nemá hook, fallback = watcher `docker events` na JODA (container start s labelem `ol1n.app=<name>`) → stejná interní událost.

---

## 3. Jak to bude fungovat (architektura)

```
                 ┌──────────────────────────────────────────────┐
  GitHub Actions │  hooks.ol1n.com/discordbot/deploy (HMAC)      │
  deploy skripty ├──────────────┐                               │
                 │              ▼                               │
  cron (v procesu)   ┌────────────────────┐   ┌──────────────┐  │
  ┌───────────┐      │  Event Bus (chan)  │──▶│  Discord     │──┼──▶ Discord API (REST + Gateway)
  │ pollers   │─────▶│  + dedup (SQLite)  │   │  Publisher   │  │
  │ apple     │      └────────────────────┘   └──────────────┘  │
  │ google    │               │                       ▲         │
  │ github    │               ▼                       │         │
  └───────────┘      ┌────────────────────┐   ┌──────────────┐  │
                     │ Milestone engine   │   │ Reconciler   │  │  apps.yaml → kategorie/kanály
                     └────────────────────┘   └──────────────┘  │
                              LLM (LiteLLM na Sparku) pro changelog shrnutí
```

### 3.1 Moduly (Go packages)
- `cmd/ol1nbot` — main, wiring, graceful shutdown.
- `internal/config` — env + `apps.yaml` (viz 3.3), validace.
- `internal/store` — SQLite (`modernc.org/sqlite`, bez CGO), sqlc, tabulky:
  `apps`, `app_versions(store, app_id, version, build, seen_at, announced)`, `deploys`, `metrics_daily(app_id, store, date, downloads, impressions, page_views, uninstalls)`, `github_activity(repo, sha/pr_id, kind, merged_at)`, `milestones(app_id, key, reached_at)`, `discord_objects(kind, key, discord_id)` (mapování app→kategorie/kanály/status message).
- `internal/events` — typované eventy: `DeployEvent`, `StoreVersionEvent`, `NewAppEvent`, `MetricsSnapshotEvent`, `ChangelogEvent`, `MilestoneEvent`.
- `internal/sources/apple`, `sources/gplay`, `sources/github`, `sources/deployhook` — každý implementuje `Poller` (`Run(ctx) / interval`) nebo HTTP handler; vždy idempotentní (dedup přes store).
- `internal/discord` — tenký wrapper nad `github.com/bwmarrin/discordgo`: `Publisher` (embeds, threads), `Reconciler` (kanály), `Commands` (slash), `Formatter` (embed builders).
- `internal/milestones` — pravidla nad `metrics_daily` + verzemi.
- `internal/changelog` — GitHub diff za týden → prompt → LLM → markdown → Discord.
- `internal/llm` — OpenAI-compatible klient na LiteLLM (`LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL`), timeout 60 s, fallback = surový seznam commitů bez LLM.

### 3.2 Struktura Discord serveru (spravuje Reconciler)
```
📢 ANNOUNCEMENTS
   #announcements        (typ News; jen milestone + nové appky; @everyone konfigurovatelně)
🛠 INFRA
   #deploys              (všechny deploy eventy, embed per deploy, failed = červená + ping role @ops)
   #changelog            (týdenní souhrn všech repos, pondělí 09:00 Europe/Prague)
   #metrics              (denní/týdenní souhrn napříč appkami + pinnutý status board)
   #bot-log              (chyby pollerů, rate limity, reconcile diff — ať je vidět, co bot dělá)
📱 APPS
   📱 <App name>          (kategorie per appka, vzniká z apps.yaml)
      #<slug>-releases    (nové verze App Store / Play, deploye backendu appky, vlákno per verze)
      #<slug>-metrics     (týdenní karta: downloads, impressions, konverze, trend vs. minulý týden)
      #<slug>-feedback    (ruční; volitelně review z storů v Phase 3)
```
Reconcile = při startu a po `/app add|remove|sync`: porovná `apps.yaml` × `discord_objects` × skutečný stav guildy, dotvoří chybějící, nikdy **nemaže** (jen archivuje: přesun do kategorie `🗄 ARCHIVE` + přejmenování s prefixem `zz-`). Diff loguje do `#bot-log`.

### 3.3 `apps.yaml` (zdroj pravdy)
```yaml
guild_id: "123"
timezone: Europe/Prague
announce:
  everyone_on_new_app: true
  everyone_on_milestone: false
github:
  owner: lioilsources
  watch_all: false
apps:
  - slug: storyteller
    name: Storyteller
    emoji: "📖"
    ios:
      bundle_id: com.ol1n.storyteller
      asc_app_id: "6741234567"
    android:
      package: com.ol1n.storyteller
    repos: [lioilsources/storyteller, lioilsources/storyteller-api]
    services: [storyteller-api]          # názvy z deploy hooku
    milestones: [first_release, downloads_100, downloads_1000, downloads_10000, v1_0]
```
Změna souboru → `/app sync` nebo restart (soubor mountnutý, fsnotify volitelně).

### 3.4 Pollery a plán
| Poller | Interval | Co dělá |
|---|---|---|
| Apple lookup (public) | 30 min | per bundle_id: verze, releaseDate → nová verze = `StoreVersionEvent` |
| ASC versions | 2 h | stav verzí (WAITING_FOR_REVIEW, IN_REVIEW, READY_FOR_SALE, REJECTED) → změna stavu do `#<slug>-releases` |
| ASC Sales daily | denně 08:00 | units za předchozí den → `metrics_daily` |
| ASC Analytics (Phase 2) | denně 08:30 | impressions, page views, conversion |
| Google Play tracks | 30 min | production track releases → nová verze / status (inProgress rollout %) |
| Google Play GCS CSV | denně 08:15 | installs overview + store performance → `metrics_daily` |
| GitHub | týdně po 09:00 (Europe/Prague) + `/changelog now` | commits (default branch), merged PRs, releases/tags za 7 dní → LLM shrnutí |
| Metrics digest | týdně po 09:30 + denně krátký řádek v `#metrics` | souhrny + editace status boardu |
| Milestones | po každém metrics/version zápisu | vyhodnotí pravidla, jednou per (app, key) |

### 3.5 Detekce "nové appky ve storu"
- Nová appka = `slug` přibyl v `apps.yaml` **nebo** první `READY_FOR_SALE` / první production release pro appku, která ještě nemá `first_release` milestone.
- Výsledek: reconcile kanálů + embed do `#announcements` (ikona z storu, odkazy na obě stores, popis z listing) + `@everyone` dle configu.

### 3.6 Milestone engine
Pravidla deklarativně (`internal/milestones/rules.go`):
- `first_release` — první živá verze v libovolném storu.
- `downloads_N` — kumulativní downloads (Apple units + Play user installs) ≥ N.
- `v1_0` — verze ≥ 1.0.0 v libovolném storu.
- `both_stores` — živá na iOS i Androidu.
- `weekly_growth_2x` — týdenní downloads ≥ 2× minulý týden (min. 50).
- `deploys_100` — 100 úspěšných deployů služby.
Každý milestone → `#announcements` (embed, barva zlatá) + zpráva do `#<slug>-releases`. Volitelně `approval: true` → nejdřív do `#bot-log` s tlačítky Approve/Skip (human-in-the-loop, stejný princip jako u OpenClaw promo bota).

### 3.7 Slash příkazy (guild-scoped, registrace při startu)
- `/app add <slug>` — přidá appku (interaktivně modal: name, bundle_id, package, repos) → zapíše do `apps.yaml`? **Ne** — apps.yaml je v repu; příkaz zapíše do DB tabulky `apps_override` a pošle do `#bot-log` YAML snippet k přidání do repa. (Rozhodnout: pokud chce Ol1n zápis do souboru, mountnout rw a přidat `git commit` — Phase 3.)
- `/app sync` — reconcile teď.
- `/app list` — tabulka appek + verze.
- `/metrics <slug> [7d|30d]` — embed s čísly.
- `/changelog now [repo]` — spustit changelog mimo plán.
- `/deploy last <service>` — poslední deploy.
- `/bot status` — health pollerů, poslední běhy, chyby.
Oprávnění: jen role `admin` (default_member_permissions = Administrator).

### 3.8 Embed formáty (Formatter)
- **Deploy**: title `🚀 storyteller-api v1.4.2 → prod`, fields Commit (link), Duration, Notes; barva zelená/červená; footer "olin.now".
- **Store version**: title `📱 Storyteller 1.5.0 — App Store`, thumbnail ikona, fields Stav, Build, Release notes (ořez 1024), tlačítko "Otevřít v storu"; nová verze zakládá thread `Storyteller 1.5.0`.
- **Weekly metrics**: jeden embed per appka, fields Downloads (7d, Δ %), Impressions, Page views, CVR; celkový souhrn nahoře.
- **Changelog**: zpráva per repo (nebo jeden dlouhý post rozdělený po 2000 zn.), hlavička `📝 Changelog 22.–28. 9. 2026`, sekce Features / Fixes / Infra, odkazy na PR.
- **Milestone**: `🏆 Storyteller: 1 000 stažení!`, zlatá barva.

### 3.9 Spolehlivost
- Každý externí zdroj má vlastní `last_success_at`; při 3 po sobě jdoucích chybách → červená zpráva do `#bot-log` (jednou, ne spam).
- Dedup: každý event má deterministický klíč (`deploy:<service>:<version>:<commit>`, `version:<store>:<app>:<version>`); před publikací `INSERT OR IGNORE`.
- Outbox: eventy do tabulky `outbox`, publisher je posílá s retry (Discord 429 → respektovat `Retry-After`). Restart neztratí nic.
- Timezone všude `Europe/Prague`, cron `github.com/robfig/cron/v3` s `WithLocation`.
- Healthcheck `GET /healthz` pro Compose.

---

## 4. Fáze implementace

**Phase 0 — Skeleton (1 session)**
- Repo `lioilsources/ol1nbot`, `go mod`, Makefile, Dockerfile (multi-stage, distroless), `docker-compose.yml` snippet pro JODA, `.env.example`, `apps.yaml.example`.
- discordgo připojení, Gateway s intents `GUILDS`, `/bot status`, Reconciler vytvoří INFRA kanály. Ověřit ručně.

**Phase 1 — Deploye + GitHub changelog (nejrychlejší hodnota, bez store klíčů)**
- Deploy hook endpoint + HMAC, outbox, embed do `#deploys`.
- GitHub poller + LLM shrnutí + `/changelog now`.
- Reconcile per-app kategorií z `apps.yaml`.
- Caddy route `hooks.ol1n.com/discordbot/*` → kontejner.

**Phase 2 — Story (verze)**
- Apple lookup + ASC versions, Google Play tracks. Threads per verze. `first_release` milestone + announcement nové appky.

**Phase 3 — Metriky + milestones**
- ASC Sales, Play GCS CSV, `metrics_daily`, weekly digest, status board, milestone engine, `/metrics`.
- ASC Analytics (impressions) až poté — složitější async API.

**Phase 4 — Nice to have**
- Review monitoring (App Store RSS `https://itunes.apple.com/rss/customerreviews/id=<id>/json`, Play `reviews.list`) → `#<slug>-feedback`.
- Approve tlačítka pro announcements, `git commit` z `/app add`, fsnotify na apps.yaml, scheduled events.

---

## 5. Testování
- Unit: formatter (golden embeds), milestone rules (table-driven), dedup klíče, HMAC.
- Integration: fake Discord REST (httptest) pro reconcile; fixture CSV pro Play, fixture JSON pro ASC/lookup.
- E2E manuální checklist: `curl` deploy hook → embed do `#deploys` do 5 s; `/app add` → kategorie vznikla; `/changelog now` → post.
- Testovací guild: samostatný Discord server "Ol1nBot Dev" + env `DISCORD_GUILD_ID`.

---

## 6. Otevřené otázky pro Ol1na (odpovědět před Phase 1/2)
1. Jak deployuje **olin.now** — je tam místo na `curl` hook (GitHub Actions / skript), nebo použít `docker events` watcher na JODA?
2. Seznam appek a jejich bundle_id / package / ASC app id / repos do `apps.yaml` (Storyteller, Kiran, Puff, SwypeKids, … které jsou reálně v storech?).
3. Má být `#announcements` s `@everyone` pro každý milestone, nebo jen nové appky? (default v plánu: jen nové appky)
4. Chce zapisovat `/app add` rovnou do `apps.yaml` v repu (git commit z bota), nebo nechat ruční PR?
5. Přiznat botovi `Manage Roles` a dělat roli per appka (`@storyteller-fans`, pingovaná při releasu)? Default: ne, Phase 4.
6. Play Console: je GCS bucket s reporty dostupný (personal účet ho má) — potvrdit, že SA jde přidat na bucket.

---

## 7. Env přehled
```
DISCORD_BOT_TOKEN, DISCORD_APP_ID, DISCORD_GUILD_ID
DEPLOY_HOOK_SECRET, HTTP_ADDR=:8080
ASC_KEY_ID, ASC_ISSUER_ID, ASC_PRIVATE_KEY_PATH, ASC_VENDOR_NUMBER
GPLAY_SA_JSON_PATH, GPLAY_REPORTS_BUCKET
GITHUB_TOKEN
LLM_BASE_URL=http://spark:4000/v1, LLM_API_KEY, LLM_MODEL
DB_PATH=/data/ol1nbot.db, APPS_YAML=/config/apps.yaml, TZ=Europe/Prague, LOG_LEVEL=info
```
