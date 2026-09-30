-- name: ListAppOverrides :many
SELECT * FROM apps_override ORDER BY slug;

-- name: UpsertAppOverride :exec
INSERT INTO apps_override (slug, yaml, created_by, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT (slug) DO UPDATE SET yaml = excluded.yaml, created_by = excluded.created_by;

-- name: DeleteAppOverride :execrows
DELETE FROM apps_override WHERE slug = ?;

-- name: InsertAppVersion :execrows
INSERT OR IGNORE INTO app_versions (store, app_slug, version, build, release_date, seen_at, announced)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: LatestAppVersion :one
SELECT * FROM app_versions WHERE store = ? AND app_slug = ? ORDER BY seen_at DESC, version DESC LIMIT 1;

-- name: ListAppVersions :many
SELECT * FROM app_versions WHERE app_slug = ? ORDER BY seen_at;

-- name: CountAppVersions :one
SELECT COUNT(*) FROM app_versions WHERE store = ? AND app_slug = ?;

-- name: GetVersionState :one
SELECT * FROM version_states WHERE store = ? AND app_slug = ? AND version = ?;

-- name: UpsertVersionState :exec
INSERT INTO version_states (store, app_slug, version, state, detail, changed_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (store, app_slug, version) DO UPDATE SET state = excluded.state, detail = excluded.detail, changed_at = excluded.changed_at;

-- name: InsertDeploy :execrows
INSERT OR IGNORE INTO deploys (service, env, version, commit_sha, url, status, notes, duration_sec, received_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: LastDeploy :one
SELECT * FROM deploys WHERE service = ? ORDER BY received_at DESC, id DESC LIMIT 1;

-- name: CountSuccessfulDeploys :one
SELECT COUNT(*) FROM deploys WHERE service = ? AND status = 'success';

-- name: UpsertMetricsDaily :exec
INSERT INTO metrics_daily (app_slug, store, date, downloads, impressions, page_views, uninstalls, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (app_slug, store, date) DO UPDATE SET
    downloads = excluded.downloads,
    impressions = CASE WHEN excluded.impressions > 0 THEN excluded.impressions ELSE metrics_daily.impressions END,
    page_views = CASE WHEN excluded.page_views > 0 THEN excluded.page_views ELSE metrics_daily.page_views END,
    uninstalls = CASE WHEN excluded.uninstalls > 0 THEN excluded.uninstalls ELSE metrics_daily.uninstalls END,
    updated_at = excluded.updated_at;

-- name: UpsertStorePerformance :exec
INSERT INTO metrics_daily (app_slug, store, date, impressions, page_views, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (app_slug, store, date) DO UPDATE SET
    impressions = excluded.impressions,
    page_views = excluded.page_views,
    updated_at = excluded.updated_at;

-- name: SumMetrics :one
SELECT CAST(COALESCE(SUM(downloads), 0) AS INTEGER) AS downloads,
       CAST(COALESCE(SUM(impressions), 0) AS INTEGER) AS impressions,
       CAST(COALESCE(SUM(page_views), 0) AS INTEGER) AS page_views,
       CAST(COALESCE(SUM(uninstalls), 0) AS INTEGER) AS uninstalls
FROM metrics_daily WHERE app_slug = ? AND date >= ? AND date <= ?;

-- name: SumMetricsByStore :many
SELECT store,
       CAST(COALESCE(SUM(downloads), 0) AS INTEGER) AS downloads,
       CAST(COALESCE(SUM(impressions), 0) AS INTEGER) AS impressions,
       CAST(COALESCE(SUM(page_views), 0) AS INTEGER) AS page_views,
       CAST(COALESCE(SUM(uninstalls), 0) AS INTEGER) AS uninstalls
FROM metrics_daily WHERE app_slug = ? AND date >= ? AND date <= ?
GROUP BY store ORDER BY store;

-- name: TotalDownloads :one
SELECT CAST(COALESCE(SUM(downloads), 0) AS INTEGER) FROM metrics_daily WHERE app_slug = ?;

-- name: InsertGithubActivity :execrows
INSERT OR IGNORE INTO github_activity (repo, kind, ref_id, title, url, author, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetMilestone :one
SELECT * FROM milestones WHERE app_slug = ? AND key = ?;

-- name: InsertMilestone :execrows
INSERT OR IGNORE INTO milestones (app_slug, key, reached_at, announced) VALUES (?, ?, ?, ?);

-- name: ListMilestones :many
SELECT * FROM milestones WHERE app_slug = ? ORDER BY reached_at;

-- name: GetDiscordObject :one
SELECT * FROM discord_objects WHERE kind = ? AND key = ?;

-- name: UpsertDiscordObject :exec
INSERT INTO discord_objects (kind, key, discord_id, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (kind, key) DO UPDATE SET discord_id = excluded.discord_id, updated_at = excluded.updated_at;

-- name: ListDiscordObjects :many
SELECT * FROM discord_objects ORDER BY kind, key;

-- name: InsertOutbox :execrows
INSERT OR IGNORE INTO outbox (dedup_key, kind, target, payload, next_attempt_at, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: DueOutbox :many
SELECT * FROM outbox WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY id LIMIT ?;

-- name: MarkOutboxSent :exec
UPDATE outbox SET status = 'sent', sent_at = ?, message_id = ?, attempts = attempts + 1, last_error = '' WHERE id = ?;

-- name: MarkOutboxRetry :exec
UPDATE outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?, status = ? WHERE id = ?;

-- name: OutboxStats :one
SELECT CAST(COALESCE(SUM(status = 'pending'), 0) AS INTEGER) AS pending,
       CAST(COALESCE(SUM(status = 'dead'), 0) AS INTEGER) AS dead,
       CAST(COALESCE(SUM(status = 'sent'), 0) AS INTEGER) AS sent
FROM outbox;

-- name: GetSourceHealth :one
SELECT * FROM source_health WHERE source = ?;

-- name: ListSourceHealth :many
SELECT * FROM source_health ORDER BY source;

-- name: UpsertSourceHealth :exec
INSERT INTO source_health (source, last_run_at, last_success_at, consecutive_failures, last_error, alerted)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (source) DO UPDATE SET
    last_run_at = excluded.last_run_at,
    last_success_at = excluded.last_success_at,
    consecutive_failures = excluded.consecutive_failures,
    last_error = excluded.last_error,
    alerted = excluded.alerted;

-- name: InsertBaseline :execrows
INSERT OR IGNORE INTO baselines (scope, key, created_at) VALUES (?, ?, ?);

-- name: HasBaseline :one
SELECT COUNT(*) FROM baselines WHERE scope = ? AND key = ?;
