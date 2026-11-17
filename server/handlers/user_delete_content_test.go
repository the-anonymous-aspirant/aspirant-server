package handlers

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// Deleting a user removes their personal content and unlinks them from shared
// rooms, in the same transaction as the soft delete (system_3 #11384, security
// c70010 on #11380, groups 1 and 2). Every value here is invented.

// userContentModels is every table User.DeleteUser deletes from or unlinks
// in (#11384). Any test harness that deletes a user migrates them, as a real
// database has them; a missing one is an error that rolls the delete back.
func userContentModels() []interface{} {
	return []interface{}{
		&data_models.Scratchpad{},
		&data_models.GoalTree{}, &data_models.GoalNode{}, &data_models.GoalEdge{}, &data_models.GoalComment{},
		&data_models.UserDisplayName{}, &data_models.UserToken{},
		&data_models.ConstellationProfile{}, &data_models.PlayerGoal{},
		&data_models.Room{}, &data_models.RoomMember{}, &data_models.Relationship{},
		&data_models.EasterHuntClick{}, &data_models.EasterHuntScore{},
		&data_models.GameScore{},
	}
}

func setupDeleteContentDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.AutoMigrate(&data_models.Role{}, &data_models.User{}, &data_models.UserFormValues{})
	db.AutoMigrate(userContentModels()...)
	t.Cleanup(func() { db.Close() })
	return db
}

func mustCreate(t *testing.T, db *gorm.DB, v interface{}) {
	t.Helper()
	if err := db.Create(v).Error; err != nil {
		t.Fatalf("seed %T: %v", v, err)
	}
}

// seedPersonal gives `u` one row in every per-user table and a goal tree with
// a node and an edge. `n` keeps unique columns apart between users.
func seedPersonal(t *testing.T, db *gorm.DB, u data_models.User, n int) (tree data_models.GoalTree, node data_models.GoalNode) {
	t.Helper()
	mustCreate(t, db, &data_models.Scratchpad{UserID: u.ID, Content: "invented note"})
	tree = data_models.GoalTree{UserID: u.ID, Name: fmt.Sprintf("Invented tree %d", n)}
	mustCreate(t, db, &tree)
	node = data_models.GoalNode{TreeID: tree.ID, Name: "Invented goal", NodeType: "goal", Body: "invented body"}
	mustCreate(t, db, &node)
	child := data_models.GoalNode{TreeID: tree.ID, Name: "Invented step", NodeType: "goal"}
	mustCreate(t, db, &child)
	mustCreate(t, db, &data_models.GoalEdge{TreeID: tree.ID, FromID: node.ID, ToID: child.ID})
	mustCreate(t, db, &data_models.UserDisplayName{UserID: u.ID, DisplayName: fmt.Sprintf("Invented %d", n), ValidFrom: time.Now()})
	mustCreate(t, db, &data_models.UserToken{UserID: u.ID, Purpose: "password-reset", TokenHash: fmt.Sprintf("invented-hash-%d", n), ExpiresAt: time.Now().Add(time.Hour)})
	mustCreate(t, db, &data_models.ConstellationProfile{UserID: u.ID, GameUsername: fmt.Sprintf("invented%d", n)})
	mustCreate(t, db, &data_models.PlayerGoal{RoomID: 1, UserID: u.ID, GoalCardID: 1})
	mustCreate(t, db, &data_models.EasterHuntClick{GameID: 1, UserID: u.ID, X: n, Y: n})
	mustCreate(t, db, &data_models.EasterHuntScore{GameID: 1, UserID: u.ID, Score: 3})
	mustCreate(t, db, &data_models.GameScore{UserID: int(u.ID), Game: "invented-game", Score: 7})
	return tree, node
}

// count reads rows Unscoped, so a soft-deleted row still counts: "deleted"
// here means gone, not marked.
func count(t *testing.T, db *gorm.DB, model interface{}, where string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.Unscoped().Model(model).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %T: %v", model, err)
	}
	return n
}

func TestDeletingAUserRemovesTheirContentAndUnlinksSharedRooms(t *testing.T) {
	db := setupDeleteContentDB(t)
	role := data_models.Role{RoleName: "Member"}
	mustCreate(t, db, &role)
	gone := data_models.User{Username: "invented-gone", Email: "gone@example.test", RoleID: role.ID}
	mustCreate(t, db, &gone)
	kept := data_models.User{Username: "invented-kept", Email: "kept@example.test", RoleID: role.ID}
	mustCreate(t, db, &kept)

	goneTree, goneNode := seedPersonal(t, db, gone, 1)
	keptTree, keptNode := seedPersonal(t, db, kept, 2)

	// Comments both ways: each user on their own tree and on the other's.
	mustCreate(t, db, &data_models.GoalComment{NodeID: goneNode.ID, UserID: gone.ID, Body: "invented"})
	mustCreate(t, db, &data_models.GoalComment{NodeID: goneNode.ID, UserID: kept.ID, Body: "on gone's tree"})
	mustCreate(t, db, &data_models.GoalComment{NodeID: keptNode.ID, UserID: gone.ID, Body: "on kept's tree"})
	keptOwn := data_models.GoalComment{NodeID: keptNode.ID, UserID: kept.ID, Body: "invented"}
	mustCreate(t, db, &keptOwn)

	// A room the deleted user created, shared with the kept user.
	room := data_models.Room{Code: "INV01", PlayerCount: 2, Status: "active", CreatorUserID: gone.ID}
	mustCreate(t, db, &room)
	mustCreate(t, db, &data_models.RoomMember{RoomID: room.ID, UserID: gone.ID, Slot: 1})
	mustCreate(t, db, &data_models.RoomMember{RoomID: room.ID, UserID: kept.ID, Slot: 2})

	// The kept user's rows, counted before the delete. User.AfterCreate opens
	// an initial display-name row, so fixed expectations would miscount.
	keptChecks := []struct {
		name  string
		model interface{}
		where string
		arg   interface{}
	}{
		{"scratchpads", &data_models.Scratchpad{}, "user_id = ?", kept.ID},
		{"goal_trees", &data_models.GoalTree{}, "user_id = ?", kept.ID},
		{"goal_nodes", &data_models.GoalNode{}, "tree_id = ?", keptTree.ID},
		{"goal_edges", &data_models.GoalEdge{}, "tree_id = ?", keptTree.ID},
		{"their own comment on their tree", &data_models.GoalComment{}, "id = ?", keptOwn.ID},
		{"user_display_names", &data_models.UserDisplayName{}, "user_id = ?", kept.ID},
		{"user_tokens", &data_models.UserToken{}, "user_id = ?", kept.ID},
		{"constellation_profiles", &data_models.ConstellationProfile{}, "user_id = ?", kept.ID},
		{"player_goals", &data_models.PlayerGoal{}, "user_id = ?", kept.ID},
		{"easter_hunt_clicks", &data_models.EasterHuntClick{}, "user_id = ?", kept.ID},
		{"easter_hunt_scores", &data_models.EasterHuntScore{}, "user_id = ?", kept.ID},
		{"game_scores", &data_models.GameScore{}, "user_id = ?", int(kept.ID)},
	}
	keptBefore := make([]int, len(keptChecks))
	for i, tc := range keptChecks {
		keptBefore[i] = count(t, db, tc.model, tc.where, tc.arg)
		if keptBefore[i] == 0 {
			t.Fatalf("seed: the kept user has no %s, so the check below proves nothing", tc.name)
		}
	}
	// Positive control the other way: the deleted user has rows to remove.
	if n := count(t, db, &data_models.UserDisplayName{}, "user_id = ?", gone.ID); n == 0 {
		t.Fatal("seed: the deleted user has no display name")
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", uint(999)) // an admin who is neither user
		c.Next()
	})
	r.DELETE("/users/:id", DeleteUserHandler)
	if w := fvDo(t, r, "DELETE", fmt.Sprintf("/users/%d", gone.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", w.Code, w.Body.String())
	}

	// Group 1: nothing of the deleted user's is left, counted Unscoped.
	for _, tc := range []struct {
		name  string
		model interface{}
		where string
		arg   interface{}
	}{
		{"scratchpads", &data_models.Scratchpad{}, "user_id = ?", gone.ID},
		{"goal_trees", &data_models.GoalTree{}, "user_id = ?", gone.ID},
		{"goal_nodes of their tree", &data_models.GoalNode{}, "tree_id = ?", goneTree.ID},
		{"goal_edges of their tree", &data_models.GoalEdge{}, "tree_id = ?", goneTree.ID},
		{"goal_comments on their tree (anyone's)", &data_models.GoalComment{}, "node_id = ?", goneNode.ID},
		{"goal_comments they wrote anywhere", &data_models.GoalComment{}, "user_id = ?", gone.ID},
		{"user_display_names", &data_models.UserDisplayName{}, "user_id = ?", gone.ID},
		{"user_tokens", &data_models.UserToken{}, "user_id = ?", gone.ID},
		{"constellation_profiles", &data_models.ConstellationProfile{}, "user_id = ?", gone.ID},
		{"player_goals", &data_models.PlayerGoal{}, "user_id = ?", gone.ID},
		{"easter_hunt_clicks", &data_models.EasterHuntClick{}, "user_id = ?", gone.ID},
		{"easter_hunt_scores", &data_models.EasterHuntScore{}, "user_id = ?", gone.ID},
		{"game_scores", &data_models.GameScore{}, "user_id = ?", int(gone.ID)},
		// Group 2: the membership row goes.
		{"constellation_room_members", &data_models.RoomMember{}, "user_id = ?", gone.ID},
	} {
		if n := count(t, db, tc.model, tc.where, tc.arg); n != 0 {
			t.Errorf("%s: %d row(s) left for the deleted user, want 0", tc.name, n)
		}
	}

	// Group 2: the room survives without pointing at the deleted user.
	var after data_models.Room
	if err := db.First(&after, room.ID).Error; err != nil {
		t.Fatalf("the shared room did not survive: %v", err)
	}
	if after.CreatorUserID != 0 {
		t.Errorf("room creator = %d, want 0 (no creator recorded)", after.CreatorUserID)
	}
	if n := count(t, db, &data_models.RoomMember{}, "room_id = ? AND user_id = ?", room.ID, kept.ID); n != 1 {
		t.Errorf("the other member's seat: %d row(s), want 1", n)
	}

	// The second user's rows are all intact.
	for i, tc := range keptChecks {
		if n := count(t, db, tc.model, tc.where, tc.arg); n != keptBefore[i] {
			t.Errorf("kept user's %s: %d row(s), want %d as before the delete", tc.name, n, keptBefore[i])
		}
	}
	// The kept user's comment on the deleted user's tree went with that tree;
	// that is the only row of theirs that should be gone.
	if n := count(t, db, &data_models.GoalComment{}, "user_id = ?", kept.ID); n != 1 {
		t.Errorf("kept user's comments: %d, want 1 (only the one on their own tree)", n)
	}

	// And the account itself is soft-deleted, as before.
	var u data_models.User
	db.Unscoped().First(&u, gone.ID)
	if u.DeletedAt == nil {
		t.Error("the user row was not soft-deleted")
	}
}

// Deleting a user removes every relationship edge that names them, in every
// direction and whether or not it was cleared, and leaves the other players'
// edges alone (system_3 #11427, security c70255 on #11426). Before this, the
// other players in a room were still served edges to a user with no seat.
func TestDeletingAUserRemovesTheirRelationshipEdges(t *testing.T) {
	db := setupDeleteContentDB(t)
	// What BuildRoomState reads besides the tables a delete touches.
	db.AutoMigrate(&data_models.RelationshipType{}, &data_models.RelationshipAction{}, &data_models.DiceRoll{}, &data_models.GoalCard{})
	role := data_models.Role{RoleName: "Member"}
	mustCreate(t, db, &role)
	gone := data_models.User{Username: "invented-gone", Email: "gone@example.test", RoleID: role.ID}
	mustCreate(t, db, &gone)
	kept := data_models.User{Username: "invented-kept", Email: "kept@example.test", RoleID: role.ID}
	mustCreate(t, db, &kept)
	third := data_models.User{Username: "invented-third", Email: "third@example.test", RoleID: role.ID}
	mustCreate(t, db, &third)

	kind := data_models.RelationshipType{Code: "X", Label: "Invented", Colour: "#123456", DisplayOrder: 1}
	mustCreate(t, db, &kind)
	room := data_models.Room{Code: "INV02", PlayerCount: 3, Status: "active", CreatorUserID: kept.ID}
	mustCreate(t, db, &room)
	for slot, u := range []data_models.User{gone, kept, third} {
		mustCreate(t, db, &data_models.RoomMember{RoomID: room.ID, UserID: u.ID, Slot: slot + 1})
	}
	// A second room, so "every room" is not just the one being read.
	other := data_models.Room{Code: "INV03", PlayerCount: 2, Status: "active", CreatorUserID: third.ID}
	mustCreate(t, db, &other)

	cleared := time.Now().Add(-time.Hour)
	edge := func(roomID, from, to uint, clearedAt *time.Time) {
		t.Helper()
		mustCreate(t, db, &data_models.Relationship{RoomID: roomID, FromUserID: from, ToUserID: to, TypeID: kind.ID, ClearedAt: clearedAt})
	}
	edge(room.ID, gone.ID, kept.ID, nil)       // the deleted user as the source
	edge(room.ID, third.ID, gone.ID, nil)      // and as the target
	edge(room.ID, gone.ID, third.ID, &cleared) // a cleared edge names them too
	edge(other.ID, kept.ID, gone.ID, nil)      // in a room they have no seat in
	edge(room.ID, kept.ID, third.ID, nil)      // two other players: this one stays

	const namesGone = "from_user_id = ? OR to_user_id = ?"
	if n := count(t, db, &data_models.Relationship{}, namesGone, gone.ID, gone.ID); n != 4 {
		t.Fatalf("seed: %d edge(s) name the deleted user, want 4", n)
	}
	// Positive control on the read: before the delete, the kept user is served
	// the edge to the deleted user. Without this, an empty list after the
	// delete could mean the read never returned such edges at all.
	namingGone := func(viewer uint) (naming, total int) {
		t.Helper()
		var current data_models.Room
		if err := db.First(&current, room.ID).Error; err != nil {
			t.Fatalf("read the room: %v", err)
		}
		state, err := data_models.BuildRoomState(db, current, viewer)
		if err != nil {
			t.Fatalf("BuildRoomState for user %d: %v", viewer, err)
		}
		for _, r := range state.Relationships {
			if r.FromUserID == gone.ID || r.ToUserID == gone.ID {
				naming++
			}
		}
		return naming, len(state.Relationships)
	}
	if naming, total := namingGone(kept.ID); naming != 1 || total != 2 {
		t.Fatalf("before the delete the kept user sees %d edge(s) naming the deleted user of %d, want 1 of 2", naming, total)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Set("user_id", uint(999)) // an admin who is none of the three
		c.Next()
	})
	r.DELETE("/users/:id", DeleteUserHandler)
	if w := fvDo(t, r, "DELETE", fmt.Sprintf("/users/%d", gone.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", w.Code, w.Body.String())
	}

	// Counted Unscoped: gone, not marked.
	if n := count(t, db, &data_models.Relationship{}, namesGone, gone.ID, gone.ID); n != 0 {
		t.Errorf("%d edge(s) still name the deleted user, want 0", n)
	}
	// The edge between the two other players is intact and still active.
	var survivors []data_models.Relationship
	if err := db.Find(&survivors).Error; err != nil {
		t.Fatalf("read the remaining edges: %v", err)
	}
	if len(survivors) != 1 || survivors[0].FromUserID != kept.ID || survivors[0].ToUserID != third.ID || survivors[0].ClearedAt != nil {
		t.Errorf("remaining edges = %+v, want exactly the active kept-to-third edge", survivors)
	}
	// What the remaining members are served.
	for _, viewer := range []data_models.User{kept, third} {
		if naming, total := namingGone(viewer.ID); naming != 0 || total != 1 {
			t.Errorf("%s sees %d edge(s) naming the deleted user of %d, want 0 of 1", viewer.Username, naming, total)
		}
	}
}
