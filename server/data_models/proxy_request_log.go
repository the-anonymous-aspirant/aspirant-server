package data_models

import "time"

// ProxyRequestLog is one row per outbound proxied call made by aspirant-server's
// handler clients. system_3 #5953.
//
// Upstream is the join key agreed with #5931's ceiling registry
// (registry.upstream_key), so item 2's margin detector joins
// proxy_request_log.upstream = registry.upstream_key with no translation layer:
//
//	commander.valuation (300s), commander.decide (15s), advisor (300s),
//	voice (120s), browser (60s), monitor (30s), translator (30s).
//
// The registry's eighth key, nginx.api, is the browser→nginx→server edge that
// sits in FRONT of aspirant-server — not one of these outbound clients — so no
// row here carries it; its latency comes from a different source.
//
// Shape: it is deliberately request_log-shaped — mirroring the columns
// system_3's backend/health_backend.py::_read_api_latency aggregates
// (route, status_code, latency_ms, created_at) — so the consumer (#5931) can
// compute per-upstream p50/p95/p99 latency by cloning that reader's
// percentile_cont SQL rather than inventing a second shape. TimeoutMs records
// the client's timeout ceiling on every row, so a consumer can compute the
// margin between observed latency and the ceiling without hardcoding a second
// copy of the per-client ceiling table.
//
// Where it lives: this row is written to aspirant-server's OWN database
// (aspirant_db), NOT system_3's system3 DB. The two run on separate Postgres
// instances on the cell — aspirant_db on 127.0.0.1:5432, system3 on
// 127.0.0.1:5434 — so there is no single "shared" Postgres to write into.
// aspirant-server already owns and AutoMigrates aspirant_db, which makes it the
// natural, low-coupling home; both instances are on the cell loopback, so
// #5931's reader opens a DSN to :5432/aspirant_db rather than reusing
// system_3's DATABASE_URL (which resolves to :5434/system3).
type ProxyRequestLog struct {
	ID         uint      `gorm:"primary_key"`
	Route      string    `gorm:"index"`
	Upstream   string    `gorm:"index"`
	StatusCode int       // HTTP status; 0 when the call errored before a response (timeout / connection refusal)
	LatencyMs  int64     `gorm:"column:latency_ms"`
	TimeoutMs  int64     `gorm:"column:timeout_ms"` // the client's timeout ceiling, so margin = timeout_ms - latency needs no second table
	CreatedAt  time.Time `gorm:"index"`
}

// TableName pins the table name rather than relying on gorm's pluraliser, since
// this is a migration surface a cross-service reader queries by name.
func (ProxyRequestLog) TableName() string { return "proxy_request_log" }
