package handlers

import (
	"aspirant-online/server/data_models"

	"github.com/jinzhu/gorm"
)

// cardgenSystemActorName is the display string for the system actor (user id
// 0): the seeds carry CreatedByUserID 0 (cardgen_template.go), and no user row
// has that id.
const cardgenSystemActorName = "system"

// cardgenActorNames resolves the display names behind a set of user ids for
// the cardgen history and asset DTOs (system_3 #8758): id 0 maps to "system",
// every other id to the user's current public display name through
// data_models.CurrentDisplayNames (one batch query; the login-username
// fallback is that helper's, for a user without an open display-name row).
// An id that resolves to no user at all is absent from the map, and the DTO
// builders render it as "" so the client keeps its "user N" fallback. The
// routes that call this are admin-only; the name is the same string the
// leaderboards already publish.
func cardgenActorNames(db *gorm.DB, ids []uint) map[uint]string {
	out := make(map[uint]string, len(ids)+1)
	seen := make(map[uint]bool, len(ids))
	var lookup []uint
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if id == 0 {
			out[0] = cardgenSystemActorName
			continue
		}
		lookup = append(lookup, id)
	}
	for id, name := range data_models.CurrentDisplayNames(db, lookup) {
		out[id] = name
	}
	return out
}

// cardgenActorName resolves one id (the single-row responses).
func cardgenActorName(db *gorm.DB, id uint) string {
	return cardgenActorNames(db, []uint{id})[id]
}
