package handlers

import (
	"strings"
	"testing"
)

// The suite runs on SQLite, which has no pg_stat_user_tables, so this guards
// the query text instead of executing it (system_3 #10686). A table looked up
// by name is resolved through server's search_path, which leaves out the
// per-service schemas, so the statement fails on the first table outside
// public. The fixed SQL was run against the live aspirant_db on #10686.
func TestDBStatsQuerySizesTablesByOIDNotName(t *testing.T) {
	q := strings.Join(strings.Fields(dbStatsQuery), " ")
	for _, banned := range []string{"quote_ident(", "::regclass", "pg_total_relation_size(relname)"} {
		if strings.Contains(q, banned) {
			t.Errorf("dbStatsQuery resolves tables by name (%q); size them by relid: %s", banned, q)
		}
	}
	for _, want := range []string{"pg_total_relation_size(relid)", "schemaname AS schema"} {
		if !strings.Contains(q, want) {
			t.Errorf("dbStatsQuery lacks %q: %s", want, q)
		}
	}
}
