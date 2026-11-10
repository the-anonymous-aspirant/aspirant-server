package handlers

import (
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func newRollupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.AutoMigrate(&data_models.ProxyRequestLog{}, &data_models.ServiceUsageRollup{}, &data_models.SitupEntry{})
	return db
}

func TestShouldRunDailyRollup(t *testing.T) {
	cases := []struct {
		name        string
		now         time.Time
		lastRunDate string
		want        bool
	}{
		{"wrong hour", time.Date(2026, 11, 10, 2, 59, 0, 0, time.UTC), "", false},
		{"target hour, never run", time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC), "", true},
		{"target hour, already run today", time.Date(2026, 11, 10, 3, 30, 0, 0, time.UTC), "2026-11-10", false},
		{"target hour, ran yesterday", time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC), "2026-11-09", true},
		{"hour after target, already run today", time.Date(2026, 11, 10, 4, 0, 0, 0, time.UTC), "2026-11-10", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRunDailyRollup(c.now, c.lastRunDate, rollupTargetHour); got != c.want {
				t.Errorf("shouldRunDailyRollup(%v, %q, %d) = %v, want %v", c.now, c.lastRunDate, rollupTargetHour, got, c.want)
			}
		})
	}
}

func TestServiceNameFromContainer(t *testing.T) {
	cases := map[string]string{
		"aspirant-online-commander-1": "commander",
		"aspirant-online-server-1":    "server",
		"commander-test-pg5907":       "commander-test-pg5907", // no project prefix/suffix match: kept raw, never mangled
	}
	for in, want := range cases {
		if got := serviceNameFromContainer(in); got != want {
			t.Errorf("serviceNameFromContainer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountAndLastUsed(t *testing.T) {
	db := newRollupTestDB(t)

	count, lastUsed, err := countAndLastUsed(db, "situp_entries", "updated_at")
	if err != nil {
		t.Fatalf("countAndLastUsed on empty table: %v", err)
	}
	if count != 0 {
		t.Errorf("empty table count = %d, want 0", count)
	}
	if lastUsed != nil {
		t.Errorf("empty table last_used = %v, want nil", lastUsed)
	}

	older := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	entries := []data_models.SitupEntry{
		{Date: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: older, CreatedAt: older},
		{Date: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), UpdatedAt: newer, CreatedAt: older},
	}
	for i := range entries {
		if err := db.Create(&entries[i]).Error; err != nil {
			t.Fatalf("seed situp entry: %v", err)
		}
	}

	count, lastUsed, err = countAndLastUsed(db, "situp_entries", "updated_at")
	if err != nil {
		t.Fatalf("countAndLastUsed: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if lastUsed == nil || !lastUsed.Equal(newer) {
		t.Errorf("last_used = %v, want %v", lastUsed, newer)
	}
}

func TestProxyRequestCounts(t *testing.T) {
	db := newRollupTestDB(t)
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	rows := []data_models.ProxyRequestLog{
		{Upstream: "advisor", Source: "member", CreatedAt: since.Add(24 * time.Hour)},
		{Upstream: "advisor", Source: "smoke", CreatedAt: since.Add(48 * time.Hour)},
		{Upstream: "advisor", Source: "member", CreatedAt: since.Add(72 * time.Hour)},
		// Outside the window: must not be counted at all.
		{Upstream: "advisor", Source: "member", CreatedAt: since.Add(-time.Hour)},
		// A different upstream: must not leak into advisor's counts.
		{Upstream: "browser", Source: "member", CreatedAt: since.Add(24 * time.Hour)},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed proxy_request_log row: %v", err)
		}
	}

	total, member, lastUsedMember, err := proxyRequestCounts(db, "advisor", since)
	if err != nil {
		t.Fatalf("proxyRequestCounts: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if member != 2 {
		t.Errorf("member = %d, want 2", member)
	}
	want := since.Add(72 * time.Hour)
	if lastUsedMember == nil || !lastUsedMember.Equal(want) {
		t.Errorf("lastUsedMember = %v, want %v (the smoke row's later timestamp must not count)", lastUsedMember, want)
	}
}

// TestRunServiceUsageRollupFeatureTablesSurviveMonitorFailure: fetchMonitorContainers
// has no real monitor to call in this test process, so it errors — this must
// not take down the feature-table half of the rollup (c64832's "partial is
// better than nothing"), and it must write a value, not a lost row, for the one
// feature table this test DB actually has.
func TestRunServiceUsageRollupFeatureTablesSurviveMonitorFailure(t *testing.T) {
	db := newRollupTestDB(t)
	entry := data_models.SitupEntry{Date: time.Now().UTC(), UpdatedAt: time.Now().UTC(), CreatedAt: time.Now().UTC()}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatalf("seed situp entry: %v", err)
	}

	runServiceUsageRollup(db)

	var row data_models.ServiceUsageRollup
	if err := db.Where("service_name = ? AND kind = ?", "situp_entries", "feature").First(&row).Error; err != nil {
		t.Fatalf("expected a feature row for situp_entries, got: %v", err)
	}
	if row.RequestCount == nil || *row.RequestCount != 1 {
		t.Errorf("situp_entries request_count = %v, want 1", row.RequestCount)
	}

	var containerRows int
	db.Model(&data_models.ServiceUsageRollup{}).Where("kind = ?", "container").Count(&containerRows)
	if containerRows != 0 {
		t.Errorf("expected no container rows when monitor is unreachable, got %d", containerRows)
	}

	// Security review (c64842 on #10505): last_used_at must land at day
	// precision, never to the second, with few members a second-precision
	// timestamp is close to logging exactly when one person was last online.
	if row.LastUsedAt == nil {
		t.Fatal("expected a non-nil last_used_at")
	}
	if h, m, s := row.LastUsedAt.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("last_used_at = %v, want midnight (day precision only)", row.LastUsedAt)
	}
}

func TestTruncateToDay(t *testing.T) {
	if got := truncateLastUsedToDay(nil); got != nil {
		t.Errorf("truncateLastUsedToDay(nil) = %v, want nil", got)
	}
	in := time.Date(2026, 11, 10, 23, 59, 59, 999, time.UTC)
	want := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	got := truncateLastUsedToDay(&in)
	if got == nil || !got.Equal(want) {
		t.Errorf("truncateLastUsedToDay(%v) = %v, want %v", in, got, want)
	}
}

func TestRunServiceUsageRollupSafelyRecoversFromPanic(t *testing.T) {
	// A nil *gorm.DB makes every query inside runServiceUsageRollup panic —
	// this must be caught, not crash the process (security review, c64842
	// on #10505).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("runServiceUsageRollupSafely let a panic escape: %v", r)
		}
	}()
	runServiceUsageRollupSafely(nil)
}
