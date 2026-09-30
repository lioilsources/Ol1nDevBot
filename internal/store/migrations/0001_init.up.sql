-- Timestamps are unix seconds (INTEGER), dates are 'YYYY-MM-DD' (TEXT, Europe/Prague).

-- Apps added at runtime via /app add. apps.yaml stays the source of truth;
-- overrides are merged on top and exported as a YAML snippet to #bot-log.
CREATE TABLE apps_override (
    slug       TEXT PRIMARY KEY,
    yaml       TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- Live versions seen in a store (appstore | playstore).
CREATE TABLE app_versions (
    store        TEXT NOT NULL,
    app_slug     TEXT NOT NULL,
    version      TEXT NOT NULL,
    build        TEXT NOT NULL DEFAULT '',
    release_date TEXT NOT NULL DEFAULT '',
    seen_at      INTEGER NOT NULL,
    announced    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (store, app_slug, version)
);

-- Last known review/rollout state per version (ASC appStoreState, Play track status).
CREATE TABLE version_states (
    store      TEXT NOT NULL,
    app_slug   TEXT NOT NULL,
    version    TEXT NOT NULL,
    state      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    changed_at INTEGER NOT NULL,
    PRIMARY KEY (store, app_slug, version)
);

CREATE TABLE deploys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    service      TEXT NOT NULL,
    env          TEXT NOT NULL,
    version      TEXT NOT NULL,
    commit_sha   TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    notes        TEXT NOT NULL DEFAULT '',
    duration_sec INTEGER NOT NULL DEFAULT 0,
    received_at  INTEGER NOT NULL
);
CREATE INDEX deploys_service_idx ON deploys (service, received_at);
CREATE UNIQUE INDEX deploys_dedup_idx ON deploys (service, env, version, commit_sha, status);

CREATE TABLE metrics_daily (
    app_slug    TEXT NOT NULL,
    store       TEXT NOT NULL,
    date        TEXT NOT NULL,
    downloads   INTEGER NOT NULL DEFAULT 0,
    impressions INTEGER NOT NULL DEFAULT 0,
    page_views  INTEGER NOT NULL DEFAULT 0,
    uninstalls  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (app_slug, store, date)
);

CREATE TABLE github_activity (
    repo        TEXT NOT NULL,
    kind        TEXT NOT NULL, -- commit | pr | release
    ref_id      TEXT NOT NULL, -- sha / PR number / tag
    title       TEXT NOT NULL,
    url         TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL DEFAULT '',
    occurred_at INTEGER NOT NULL,
    PRIMARY KEY (repo, kind, ref_id)
);

CREATE TABLE milestones (
    app_slug   TEXT NOT NULL,
    key        TEXT NOT NULL,
    reached_at INTEGER NOT NULL,
    announced  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (app_slug, key)
);

-- Mapping of logical objects (category/channel/status message) to Discord snowflakes.
CREATE TABLE discord_objects (
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    discord_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (kind, key)
);

-- One row per (event, target channel). dedup_key makes emission idempotent.
CREATE TABLE outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dedup_key       TEXT NOT NULL UNIQUE,
    kind            TEXT NOT NULL,
    target          TEXT NOT NULL,
    payload         TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending', -- pending | sent | dead
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    message_id      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    sent_at         INTEGER
);
CREATE INDEX outbox_pending_idx ON outbox (status, next_attempt_at);

CREATE TABLE source_health (
    source               TEXT PRIMARY KEY,
    last_run_at          INTEGER NOT NULL DEFAULT 0,
    last_success_at      INTEGER NOT NULL DEFAULT 0,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    alerted              INTEGER NOT NULL DEFAULT 0
);

-- Marks that a (scope, key) was initialised, so pre-existing state of already
-- launched apps is recorded silently instead of being announced as new.
CREATE TABLE baselines (
    scope      TEXT NOT NULL,
    key        TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (scope, key)
);
