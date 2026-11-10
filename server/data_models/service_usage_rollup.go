package data_models

import "time"

// ServiceUsageRollup is one row per served service (container or server-native
// feature), written nightly (system_3 #10490-C / #10496-B1). It exists so the
// next version of report 877 (/artifacts/877 — "which of the served
// docker-compose contents are actually being used") is a query, not a day of
// `docker exec` and ad-hoc SQL.
//
// Kind distinguishes two populations with no 1:1 correspondence: a `container`
// row is one per entry aspirant-monitor's `/containers` lists (so every served
// service gets a memory/disk figure, matching report 877's full coverage); a
// `feature` row is one per server-native feature table #10490-B's audit named
// (c64796 on #10495) — several of these share the one `server` container, so
// they carry no memory/disk of their own.
//
// Every usage figure is a pointer: nil means "not applicable to this row's
// kind" or "no signal exists for this service", never a false zero — the same
// convention #10490-A1's Source column and this repo's other nullable-signal
// fields already use.
type ServiceUsageRollup struct {
	ID          uint   `gorm:"primary_key"`
	ServiceName string `gorm:"index"`
	Kind        string `gorm:"index"` // "container" or "feature"

	MemoryMB *float64 `gorm:"column:memory_mb"`
	DiskMB   *float64 `gorm:"column:disk_mb"`

	// RequestCount is a container row's proxied-call count (grouped by
	// upstream, over the trailing window StartServiceUsageRollup uses) or a
	// feature row's whole-table count(*) — unwindowed, per #10490-B's own
	// read-time-count(*) verdict for these small tables.
	RequestCount *int64 `gorm:"column:request_count"`
	// RequestCountMember is the RequestCount subset classified `source =
	// "member"` (system_3 #10490-A1) — nil for a feature row, which carries no
	// Source distinction of its own.
	RequestCountMember *int64 `gorm:"column:request_count_member"`

	LastUsedAt *time.Time `gorm:"column:last_used_at"`

	// AsOf is this rollup run's own timestamp — every row from one run shares
	// one AsOf, so a reader can group by run without a separate run-id table.
	AsOf time.Time `gorm:"column:as_of;index"`
}

// TableName pins the table name rather than relying on gorm's pluraliser,
// since #10491's admin page reads this table by name.
func (ServiceUsageRollup) TableName() string { return "service_usage_rollups" }
