package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// Nightly per-service usage rollup (system_3 #10490-C / #10496-B1). See the
// design comment on #10502 (c64832) for the full architecture rationale: this
// lives in aspirant-server because every input it needs is either already in
// this service's own database (proxy_request_log, the server-native feature
// tables) or already reachable through a client this service already has
// (monitorClient, the same seam doProxy wraps for #10490-A1) — no new
// cross-service database credential, no new docker-socket-proxy allowlist
// entry.

// rollupTargetHour is when the nightly run fires, in UTC — matching this
// cell's existing cron cadence for similar sweeps (e.g. the docker-prune
// sweep, the activity_log partition job).
const rollupTargetHour = 3

// upstreamServiceMap joins proxy_request_log.upstream keys to the real
// container name aspirant-monitor's /containers reports for them (confirmed
// via `docker ps` 2026-11-10 — the full "aspirant-online-<service>-1" compose
// form, never a bare short name or a substring match: a leftover orphan test
// container, commander-test-pg5907, contains "commander" as a substring and
// must never fuzzy-match the real aspirant-online-commander-1).
var upstreamServiceMap = map[string]string{
	"commander.valuation": "aspirant-online-commander-1",
	"commander.decide":    "aspirant-online-commander-1",
	"advisor":             "aspirant-online-advisor-1",
	"voice":               "aspirant-online-transcriber-1",
	"browser":             "aspirant-online-browser-1",
	"monitor":             "aspirant-online-monitor-1",
	"translator":          "aspirant-online-translator-1",
}

// featureTable names a server-native feature's table and which timestamp
// column it carries. Both fields are literal Go identifiers in this file,
// never derived from input, so building SQL with them by string formatting
// (below) carries no injection risk.
type featureTable struct {
	table        string
	timestampCol string // "updated_at" or "created_at"
}

// featureTables is #10490-B's audit (c64796 on #10495), re-verified against
// the live schema 2026-11-10T22:55Z rather than trusted from memory — that
// re-check found two corrections the audit comment's prose didn't carry:
// invoicing_audit_events has only created_at (append-only rows, never
// updated), and invoicing_profiles has only updated_at (no created_at at
// all). goal_edges' created_at-only finding was already correct.
var featureTables = []featureTable{
	{"cards", "updated_at"},
	{"card_versions", "updated_at"},
	{"card_templates", "updated_at"},
	{"card_export_profiles", "updated_at"},
	{"scratchpads", "updated_at"},
	{"situp_entries", "updated_at"},
	{"pushup_entries", "updated_at"},
	{"easter_hunt_games", "updated_at"},
	{"easter_hunt_clicks", "updated_at"},
	{"constellation_rooms", "updated_at"},
	{"constellation_dice_rolls", "updated_at"},
	{"constellation_profiles", "updated_at"},
	{"constellation_relationships", "updated_at"},
	{"constellation_relationship_events", "updated_at"},
	{"constellation_relationship_actions", "updated_at"},
	{"relationship_types", "updated_at"},
	{"player_goals", "updated_at"},
	{"goal_cards", "updated_at"},
	{"goal_trees", "updated_at"},
	{"goal_nodes", "updated_at"},
	{"goal_edges", "created_at"},
	{"goal_comments", "updated_at"},
	{"messages", "updated_at"},
	{"game_scores", "updated_at"},
	{"invoicing_profiles", "updated_at"},
	{"invoicing_invoices", "updated_at"},
	{"invoicing_payments", "updated_at"},
	{"invoicing_expenses", "updated_at"},
	{"invoicing_audit_events", "created_at"},
	{"invoicing_profile_logos", "updated_at"},
	{"ludde_feeding_times", "updated_at"},
}

// rollupRequestWindow bounds the proxied-call count to a trailing window
// rather than the table's whole lifetime — the task body named no window, so
// this is this subtask's own call, matching the width this cell's other
// rolling health reads already use.
const rollupRequestWindow = 30 * 24 * time.Hour

// containerNamePattern strips this cell's own compose project prefix and
// instance suffix for a readable service_name — cosmetic, tied to THIS
// cell's "aspirant-online" project name, and safe to fall back from: a
// container name that doesn't match (a different project, a future rename)
// keeps its raw name rather than being mangled.
var containerNamePattern = regexp.MustCompile(`^aspirant-online-(.+)-\d+$`)

func serviceNameFromContainer(containerName string) string {
	if m := containerNamePattern.FindStringSubmatch(containerName); m != nil {
		return m[1]
	}
	return containerName
}

// shouldRunDailyRollup is the pure time-gating decision, factored out of the
// goroutine loop so it is unit-testable without a real clock wait: true
// exactly once per UTC calendar day, the first tick at or after
// rollupTargetHour:00.
func shouldRunDailyRollup(now time.Time, lastRunDate string, targetHour int) bool {
	return now.UTC().Hour() == targetHour && now.UTC().Format("2006-01-02") != lastRunDate
}

// monitorContainer is the subset of aspirant-monitor's /containers response
// this rollup reads. Other fields (cpu_percent, network_*, state, ...) are
// read by other consumers and not needed here.
type monitorContainer struct {
	Name       string   `json:"name"`
	MemoryMB   *float64 `json:"memory_usage_mb"`
	DiskMB     *float64 `json:"disk_mb"`
}

type monitorContainersResponse struct {
	Containers []monitorContainer `json:"containers"`
}

// fetchMonitorContainers calls monitor's /containers through the same proxy
// client/seam #10490-A1's doProxy wraps for every other handler — no new
// capability, no new credential.
func fetchMonitorContainers() ([]monitorContainer, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/containers", monitorURL()), nil)
	if err != nil {
		return nil, err
	}
	resp, err := doProxy(monitorClient, req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("monitor /containers: unexpected status %d", resp.StatusCode)
	}
	var parsed monitorContainersResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Containers, nil
}

// countAndLastUsed runs SELECT count(*), MAX(<timestampCol>) FROM <table> —
// both identifiers are always one of this file's own hardcoded literals
// (featureTables), never derived from a request, so building the query by
// string formatting carries no injection risk.
//
// LastUsed scans into interface{}, not *time.Time: Postgres's driver decodes
// MAX(timestamp) straight to time.Time, but sqlite (only ever seen here in
// this file's own tests, never in production) returns the aggregate as a
// bare string with no declared-column-type hint to parse it by, so a typed
// *time.Time destination errors on that driver. parseLastUsed normalizes
// either shape.
func countAndLastUsed(db *gorm.DB, table, timestampCol string) (int64, *time.Time, error) {
	var row struct {
		Count    int64
		LastUsed interface{}
	}
	query := fmt.Sprintf(`SELECT count(*) AS count, MAX(%s) AS last_used FROM %s`, timestampCol, table)
	if err := db.Raw(query).Scan(&row).Error; err != nil {
		return 0, nil, err
	}
	return row.Count, parseLastUsed(row.LastUsed), nil
}

// parseLastUsed normalizes a MAX(timestamp) aggregate's driver value: already
// a time.Time (Postgres), or text in one of sqlite's own stored layouts (test
// DBs only). An unrecognized shape returns nil rather than guessing.
func parseLastUsed(v interface{}) *time.Time {
	switch t := v.(type) {
	case time.Time:
		return &t
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return &parsed
			}
		}
	case []byte:
		return parseLastUsed(string(t))
	}
	return nil
}

// proxyRequestCounts reads proxy_request_log for one upstream over the
// trailing rollupRequestWindow: total count, the source='member' subset, and
// the most recent member request's timestamp — a smoke sweep's own
// timestamp is not "last used" (system_3 #10490-A1).
func proxyRequestCounts(db *gorm.DB, upstream string, since time.Time) (total int64, member int64, lastUsedMember *time.Time, err error) {
	if err = db.Model(&data_models.ProxyRequestLog{}).
		Where("upstream = ? AND created_at >= ?", upstream, since).
		Count(&total).Error; err != nil {
		return
	}
	if err = db.Model(&data_models.ProxyRequestLog{}).
		Where("upstream = ? AND created_at >= ? AND source = ?", upstream, since, "member").
		Count(&member).Error; err != nil {
		return
	}
	var row struct{ LastUsed interface{} }
	q := db.Model(&data_models.ProxyRequestLog{}).
		Select("MAX(created_at) AS last_used").
		Where("upstream = ? AND created_at >= ? AND source = ?", upstream, since, "member")
	err = q.Scan(&row).Error
	lastUsedMember = parseLastUsed(row.LastUsed)
	return
}

func ptrInt64(v int64) *int64 { return &v }

// truncateLastUsedToDay drops a last-used timestamp to its UTC calendar date
// (security review, c64842 on #10505): report 877 only needs to know
// whether a service is in use, which day precision answers, and with few
// members a second-precision `MAX(created_at)` of member-only activity is
// close to logging when one specific person was last online. Applied at
// write time so no reader needs to know to re-truncate it.
func truncateLastUsedToDay(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}

// runServiceUsageRollup performs one rollup pass: one container row per
// aspirant-monitor container, one feature row per featureTables entry, all
// sharing one AsOf timestamp.
func runServiceUsageRollup(db *gorm.DB) {
	asOf := time.Now().UTC()
	since := asOf.Add(-rollupRequestWindow)

	containers, err := fetchMonitorContainers()
	if err != nil {
		log.Printf("service usage rollup: failed to read monitor containers: %v", err)
		containers = nil // still roll up feature tables below — partial is better than nothing
	}

	// Reverse upstreamServiceMap: container name -> the upstream keys it
	// serves, since one container (commander) can answer to two upstreams.
	upstreamsByContainer := map[string][]string{}
	for upstream, container := range upstreamServiceMap {
		upstreamsByContainer[container] = append(upstreamsByContainer[container], upstream)
	}

	for _, c := range containers {
		row := data_models.ServiceUsageRollup{
			ServiceName: serviceNameFromContainer(c.Name),
			Kind:        "container",
			MemoryMB:    c.MemoryMB,
			DiskMB:      c.DiskMB,
			AsOf:        asOf,
		}
		if upstreams, ok := upstreamsByContainer[c.Name]; ok {
			var total, member int64
			var lastUsed *time.Time
			for _, upstream := range upstreams {
				t, m, lu, err := proxyRequestCounts(db, upstream, since)
				if err != nil {
					log.Printf("service usage rollup: proxy counts for %s: %v", upstream, err)
					continue
				}
				total += t
				member += m
				if lu != nil && (lastUsed == nil || lu.After(*lastUsed)) {
					lastUsed = lu
				}
			}
			row.RequestCount = ptrInt64(total)
			row.RequestCountMember = ptrInt64(member)
			row.LastUsedAt = truncateLastUsedToDay(lastUsed)
		}
		if err := db.Create(&row).Error; err != nil {
			log.Printf("service usage rollup: insert failed for container %s: %v", c.Name, err)
		}
	}

	for _, ft := range featureTables {
		count, lastUsed, err := countAndLastUsed(db, ft.table, ft.timestampCol)
		if err != nil {
			log.Printf("service usage rollup: count/last-used for %s: %v", ft.table, err)
			continue
		}
		row := data_models.ServiceUsageRollup{
			ServiceName:  ft.table,
			Kind:         "feature",
			RequestCount: ptrInt64(count),
			LastUsedAt:   truncateLastUsedToDay(lastUsed),
			AsOf:         asOf,
		}
		if err := db.Create(&row).Error; err != nil {
			log.Printf("service usage rollup: insert failed for feature %s: %v", ft.table, err)
		}
	}
}

// StartServiceUsageRollup launches the single background goroutine that
// fires runServiceUsageRollup once near rollupTargetHour:00 UTC daily,
// mirroring StartProxyLatencyWriter's own construction (#5953): a nil db
// (tests, or a boot path without a DB) makes this a no-op.
func StartServiceUsageRollup(db *gorm.DB) {
	if db == nil {
		return
	}
	go func() {
		lastRunDate := ""
		for {
			now := time.Now()
			if shouldRunDailyRollup(now, lastRunDate, rollupTargetHour) {
				runServiceUsageRollupSafely(db)
				lastRunDate = now.UTC().Format("2006-01-02")
			}
			time.Sleep(time.Minute)
		}
	}()
}

// runServiceUsageRollupSafely recovers a panic inside runServiceUsageRollup
// (security review, c64842 on #10505): without it, a panic here at 03:00
// would crash the whole server, not just skip one night's rollup.
func runServiceUsageRollupSafely(db *gorm.DB) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("service usage rollup: recovered from panic: %v", r)
		}
	}()
	runServiceUsageRollup(db)
}
