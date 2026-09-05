package server

import (
	"fmt"
	"log"

	"aspirant-online/server/handlers"
	"aspirant-online/server/middleware"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// jobsOwnerUsername is the account that owns the Jobs Member app (#4196/#4194).
// Jobs is a single-user app (vinoly's Berlin job search), so its proxy routes
// are gated to this account plus Admin rather than all-Trusted. Operator ruled
// option B (single-tenant gate, not a per-user interaction model).
const jobsOwnerUsername = "vinoly"

// pushupsOwnerUsername owns the Pappas pushup-challenge log (#4203/#4194).
// The operator ruled it robert's own private log, so its routes are gated to
// this account plus Admin — a route gate (not a per-user schema rework) because
// the log is single-user, mirroring the Jobs decision (#4196).
const pushupsOwnerUsername = "robert"

// luddeOwnerUsername owns the Ludde meal-tracker feeding log (#4195/#4194).
// The operator ruled it tied only to jenny ("Only Jenny needs this one"), so
// its routes are gated to this account plus Admin — a route gate (not a
// per-user schema rework), mirroring the Jobs (#4196) and pushups (#4203)
// single-tenant decisions. The single global feeding_times table stays as-is;
// the gate makes the existing log private to jenny rather than all-Trusted.
const luddeOwnerUsername = "jenny"

// -------------------------------------
// CORE SETUP AND INITIALIZATION
// -------------------------------------

// BuildTables initializes the database tables
func BuildTables() {
	db, err := SetupDBConnection()
	if err != nil {
		log.Fatalf("Error connecting to database: %v", err)
	}
	defer db.Close()

	AutoMigrate(db)
	log.Println("Database tables built successfully")
}

// SetupMiddleware sets up the middleware for the Gin engine
func SetupMiddleware(r *gin.Engine) {
	r.Use(cors.New(corsConfig()))
	r.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		// Custom log format including role and user
		role, _ := param.Keys["role"].(string)
		user, _ := param.Keys["user_id"].(uint)
		username, _ := param.Keys["user_name"].(string)

		return fmt.Sprintf("[GIN] %v | %3d | %15s | %-7s %#v | role: %s | user: %d | username: %s\n",
			param.TimeStamp.Format("2006-01-02T15:04:05Z"),
			param.StatusCode,
			param.ClientIP,
			param.Method,
			param.Path,
			role,
			user,
			username,
		)
	}))
	log.Println("Middleware set up successfully")
}

// -------------------------------------
// ROUTE REGISTRATION
// -------------------------------------

// RegisterRoutes sets up all the routes for the server
// It organizes routes by authentication level and functionality
func RegisterRoutes(router *gin.Engine, db *gorm.DB) {
	// Add DB to context for all routes
	router.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Next()
	})

	// Public routes - no authentication required.
	// POST /login sits behind LoginRateLimit to throttle credential-
	// stuffing and per-IP brute force (security-finding #1380).
	// GET /login/:username was removed by the same finding — it
	// disclosed username+email+role for any username to unauth
	// callers and had no client consumers.
	router.POST("/login", middleware.LoginRateLimit(), handlers.LoginHandler)
	// POST /logout is public by design: it must clear the session cookie
	// even when the token is already expired or malformed. Clearing the
	// HttpOnly auth_token cannot be done from the SPA (security-finding
	// #2589), so this route is the only way a session actually ends.
	router.POST("/logout", handlers.LogoutHandler)
	router.GET("/health", handlers.HealthCheckHandler)
	router.GET("/fetch-object/:etag", handlers.FetchObjectHandler)
	// /games/word_weaver and GET /games/scores moved to the viewer tier
	// (#5113 D1 — applications require a logged-in viewer, no longer public).
	// Bootstrap route for creating first admin user when no users exist
	router.POST("/bootstrap/admin", handlers.BootstrapUserHandler)
	// Public self-service sign-up (#5113-C1). Unauthenticated by definition —
	// the caller has no account yet. Both endpoints answer identically whether
	// or not an account exists, so neither is an existence oracle; abuse limits
	// are middleware and land in #5222.
	router.POST("/signup", handlers.SignupHandler)
	router.POST("/verify-email", handlers.VerifyEmailHandler)
	// Password recovery (#5113-C1). /password/forgot answers identically for a
	// known and an unknown address — it takes an address and nothing else, so a
	// distinguishable answer would make it a bulk address-membership oracle.
	// It is also an unauthenticated way to make this server send mail to an
	// address the caller picks, so #5222 gives it a per-address limit as well
	// as the per-IP one.
	router.POST("/password/forgot", handlers.ForgotPasswordHandler)
	router.POST("/password/reset", handlers.ResetPasswordHandler)

	// Authentication middleware
	authMiddleware := middleware.AuthMiddleware()

	// Viewer tier — applications (games, quizzes, Constellations play) and the
	// caller's own profile. Requires a logged-in viewer+ account; applications
	// are no longer anonymous-public (#5113 D1) and Blocked users fail the floor.
	authRoutes := router.Group("/")
	authRoutes.Use(authMiddleware)
	authRoutes.Use(handlers.RequireTier(handlers.TierViewer))
	{
		// GET /data_models/users(+/:id) enumerate every account, so they moved
		// up to the admin tier (#5113 D3) — a viewer must not read the roster.
		authRoutes.POST("/games/scores", handlers.SaveGameScoreHandler)
		// Word-finder game + score leaderboard — applications, viewer-gated
		// (#5113 D1; formerly anonymous-public on `router`).
		authRoutes.POST("/games/word_weaver", handlers.GetLongestWordsHandler)
		authRoutes.GET("/games/scores", handlers.GetGameScoresHandler)

		// Per-user scratchpad — any logged-in user gets their own text buffer,
		// scoped to the session user id (never a URL/body parameter).
		authRoutes.GET("/users/me/scratchpad", handlers.GetScratchpadHandler)
		authRoutes.PUT("/users/me/scratchpad", handlers.PutScratchpadHandler)

		// Own profile surface (#4170). Scoped to the session user_id — a
		// caller can only read/edit their own profile. Registered under
		// /profile rather than /data_models/users/me because a static `me`
		// segment would conflict with the /data_models/users/:id wildcard in
		// Gin's router. The per-user avatar-serve route takes an :id because
		// any authenticated caller may render any user's avatar (message board).
		authRoutes.GET("/profile", handlers.GetMeHandler)
		authRoutes.PATCH("/profile", handlers.PatchMeHandler)
		authRoutes.PUT("/profile/avatar", handlers.PutMeAvatarHandler)
		authRoutes.DELETE("/profile/avatar", handlers.DeleteMeAvatarHandler)
		authRoutes.GET("/data_models/users/:id/avatar", handlers.GetUserAvatarHandler)

		// Constellations companion app (epic #4587). The relationship-type
		// vocabulary is non-sensitive reference data any logged-in player needs
		// to render the board; colours come from the row, not the frontend.
		authRoutes.GET("/constellations/relationship-types", handlers.GetConstellationRelationshipTypesHandler)
		// Goal-card victory-condition deck (#4807-A1) — non-sensitive reference
		// data any player needs to render the dictionary; text and predicate
		// key come from the row.
		authRoutes.GET("/constellations/goal-cards", handlers.GetConstellationGoalCardsHandler)
		// Per-user game identity (subtask #4595-A3), scoped to the session user;
		// the icon reuses the existing avatar.
		authRoutes.GET("/constellations/profile", handlers.GetConstellationProfileHandler)
		authRoutes.PUT("/constellations/profile", handlers.PutConstellationProfileHandler)
	}

	// Viewer tier — Constellations room lifecycle (#5113-B1 / operator D2). The
	// game moves to the applications (viewer) tier so a signed-up viewer can
	// create/join/play. The per-room boundary — must be a member of THIS room,
	// creator-only for settings — stays enforced in the handlers (e.g.
	// constellation_state.go: "Only a member of the room may view its state"),
	// NOT by the tier gate, so lowering the gate to viewer does not let anyone
	// touch a room they are not in.
	constellationRoomRoutes := router.Group("/")
	constellationRoomRoutes.Use(authMiddleware)
	{
		constellationRoomRoutes.Use(handlers.RequireTier(handlers.TierViewer))

		// Constellations companion app (epic #4587) — room lifecycle. The
		// member app is Trusted/Admin gated (#4587-A1); create/join/leave a
		// game and read its live state.
		constellationRoomRoutes.POST("/constellations/rooms", handlers.CreateRoomHandler)
		constellationRoomRoutes.GET("/constellations/rooms/:code", handlers.GetRoomHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/join", handlers.JoinRoomHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/leave", handlers.LeaveRoomHandler)

		// Constellations room settings (#4835): the two creator-only
		// transparency toggles. Creator-only authorization is enforced
		// server-side in data_models.SetRoomReveal, not by hiding the control.
		constellationRoomRoutes.POST("/constellations/rooms/:code/settings", handlers.SetRoomSettingsHandler)

		// Constellations room live-state snapshot (#4587-D1). One aggregate
		// board read for a short-poll client; member-gated like the reads it
		// composes.
		constellationRoomRoutes.GET("/constellations/rooms/:code/state", handlers.GetRoomStateHandler)

		// Constellations room relationship-event history (#4833-A1 / #4847).
		// Oldest-first cursor pages over the #4829-A3 append-only event log,
		// scoped to the viewer's own edges like /state and /relationships
		// (#4809); member-gated. Distinct from .../relationships/history,
		// which is the caller's capped undo stack (#4599).
		constellationRoomRoutes.GET("/constellations/rooms/:code/history", handlers.GetRoomHistoryHandler)

		// Constellations relationship-graph edit API (#4587-B1). The shared
		// graph a room's members agree on; only a member of the room may edit
		// or read it (enforced in the handler on top of the Trusted gate).
		constellationRoomRoutes.GET("/constellations/rooms/:code/relationships", handlers.GetRelationshipsHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/relationships/set", handlers.SetRelationshipHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/relationships/clear", handlers.ClearRelationshipHandler)

		// Constellations relationship edit history (#4587-C1). Per-player
		// undo/redo over the shared graph, capped per player; the app enforces
		// no game rules (operator ruling c25775).
		constellationRoomRoutes.GET("/constellations/rooms/:code/relationships/history", handlers.GetRelationshipHistoryHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/relationships/undo", handlers.UndoRelationshipHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/relationships/redo", handlers.RedoRelationshipHandler)

		// Constellations goal selection (#4807-A1). A player selects their own
		// private goal card for the room; the choice appears only in that
		// player's /state (serializer-scoped), never another's. No read route —
		// a goal is read through /state, by its owner alone.
		constellationRoomRoutes.POST("/constellations/rooms/:code/goal/set", handlers.SetGoalHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/goal/clear", handlers.ClearGoalHandler)

		// Constellations server-authoritative dice (#4587-B2). Everyone in the
		// room sees the same resolved roll; only a member may roll or read it.
		constellationRoomRoutes.GET("/constellations/rooms/:code/dice", handlers.GetDiceHandler)
		constellationRoomRoutes.POST("/constellations/rooms/:code/dice/roll", handlers.RollDiceHandler)
	}

	// Member tier — the member area: files, message board, translator, goals,
	// valuation, personal apps (#5113). (Group var kept as trustedRoutes to hold
	// the diff to the gate; it now means "member+". Constellations rooms moved to
	// the viewer group above in #5113-B1.)
	trustedRoutes := router.Group("/")
	trustedRoutes.Use(authMiddleware)
	{
		trustedRoutes.Use(handlers.RequireTier(handlers.TierMember))

		// Easter Egg Hunt
		trustedRoutes.GET("/games/easter-hunt/state", handlers.GetEasterHuntStateHandler)
		trustedRoutes.GET("/games/easter-hunt/scores", handlers.GetEasterHuntScoresHandler)
		trustedRoutes.POST("/games/easter-hunt/clicks", handlers.PostEasterHuntClickHandler)
		trustedRoutes.GET("/games/easter-hunt/cooldown", handlers.GetEasterHuntCooldownHandler)

		// Message board
		trustedRoutes.GET("/data_models/message", handlers.GetAllMessagesHandler)
		trustedRoutes.POST("/data_models/message", handlers.PostMessageHandler)

		// File management routes
		trustedRoutes.GET("/files/list", handlers.ListFilesHandler)
		trustedRoutes.GET("/files/shared/list", handlers.ListSharedFilesHandler)
		trustedRoutes.POST("/files/upload", handlers.UploadFileHandler)
		trustedRoutes.POST("/files/shared/upload", handlers.UploadSharedFileHandler)
		trustedRoutes.GET("/files/download/:filename", handlers.DownloadFileHandler)
		trustedRoutes.GET("/files/shared/download/:filename", handlers.DownloadSharedFileHandler)
		trustedRoutes.DELETE("/files/delete/:filename", handlers.DeleteFileHandler)
		trustedRoutes.POST("/files/folder", handlers.CreateFolderHandler)
		trustedRoutes.POST("/files/shared/folder", handlers.CreateSharedFolderHandler)
		trustedRoutes.GET("/files/usage/me", handlers.OwnStorageUsageHandler)

		// Translator (proxied to translator service)
		trustedRoutes.GET("/translator/health", handlers.GetTranslatorHealthHandler)
		trustedRoutes.GET("/translator/languages", handlers.GetTranslatorLanguagesHandler)
		trustedRoutes.POST("/translator/languages/install", handlers.InstallTranslatorLanguageHandler)
		trustedRoutes.POST("/translator/translations", handlers.TranslateHandler)

		// Valuation Statement (proxied to commander service)
		trustedRoutes.POST("/commander/valuation-statement/extract", handlers.PostCommanderValuationExtractHandler)
		trustedRoutes.POST("/commander/valuation-statement/generate", handlers.PostCommanderValuationGenerateHandler)
		// operator-defaults (appraiser identity + default likviditet) is shared
		// operator config, not per-user data, so it is Admin-only — registered in
		// adminRoutes below, not here (system_3 #3182, follow-up to #3096).

		// Valuation Statement — processed-valuations store ('Tidigare värderingar' tab).
		// export.csv is registered BEFORE the :id route so the literal path segment wins
		// over the wildcard param matcher.
		trustedRoutes.POST("/commander/valuation-statement/processed", handlers.PostCommanderValuationProcessedHandler)
		trustedRoutes.GET("/commander/valuation-statement/processed", handlers.ListCommanderValuationProcessedHandler)
		trustedRoutes.GET("/commander/valuation-statement/processed/export.csv", handlers.ExportCommanderValuationProcessedCsvHandler)
		trustedRoutes.GET("/commander/valuation-statement/processed/:id", handlers.GetCommanderValuationProcessedHandler)
		trustedRoutes.PATCH("/commander/valuation-statement/processed/:id", handlers.UpdateCommanderValuationProcessedHandler)
		trustedRoutes.DELETE("/commander/valuation-statement/processed/:id", handlers.DeleteCommanderValuationProcessedHandler)

		// Shared image upload (used by several apps + admin assets) — stays
		// all-Trusted, not tied to any single app owner.
		trustedRoutes.POST("/upload", handlers.UploadImageHandler)

		// Ludde meal tracker — feeding times. Owned by jenny (#4195/#4194): the
		// operator ruled it a single-user app ("Only Jenny needs this one"), so
		// the routes are gated to the jenny account plus Admin, not all-Trusted.
		// The single global feeding_times table stays as-is (option B —
		// single-tenant gate, no per-user schema rework), mirroring Jobs/#4196.
		trustedRoutes.GET("/data_models/ludde_feeding_times", handlers.ValidateUserOrAdmin(luddeOwnerUsername), handlers.GetAllFeedingTimesHandler)
		trustedRoutes.GET("/data_models/ludde_feeding_times/:id", handlers.ValidateUserOrAdmin(luddeOwnerUsername), handlers.GetFeedingTimeHandler)
		trustedRoutes.POST("/data_models/ludde_feeding_times", handlers.ValidateUserOrAdmin(luddeOwnerUsername), handlers.AddFeedingTimeHandler)
		trustedRoutes.DELETE("/data_models/ludde_feeding_times/:id", handlers.ValidateUserOrAdmin(luddeOwnerUsername), handlers.DeleteFeedingTimeHandler)

		// Goal Mapper — Trees
		trustedRoutes.POST("/goals/trees", handlers.CreateTreeHandler)
		trustedRoutes.GET("/goals/trees", handlers.ListTreesHandler)
		trustedRoutes.GET("/goals/trees/:id", handlers.GetTreeHandler)
		trustedRoutes.PATCH("/goals/trees/:id", handlers.UpdateTreeHandler)
		trustedRoutes.DELETE("/goals/trees/:id", handlers.DeleteTreeHandler)

		// Goal Mapper — Session Lock
		trustedRoutes.POST("/goals/trees/:id/open", handlers.OpenTreeForEditingHandler)
		trustedRoutes.POST("/goals/trees/:id/take-over", handlers.TakeOverTreeEditingHandler)
		trustedRoutes.POST("/goals/trees/:id/release", handlers.ReleaseTreeEditingHandler)

		// Goal Mapper — Nodes
		trustedRoutes.POST("/goals/trees/:id/nodes", handlers.CreateNodeHandler)
		trustedRoutes.GET("/goals/trees/:id/nodes", handlers.ListNodesHandler)
		trustedRoutes.GET("/goals/trees/:id/nodes/:node_id", handlers.GetNodeHandler)
		trustedRoutes.PATCH("/goals/trees/:id/nodes/:node_id", handlers.UpdateNodeHandler)
		trustedRoutes.DELETE("/goals/trees/:id/nodes/:node_id", handlers.DeleteNodeHandler)
		trustedRoutes.POST("/goals/trees/:id/nodes/:node_id/complete", handlers.CompleteNodeHandler)
		trustedRoutes.POST("/goals/trees/:id/nodes/:node_id/uncomplete", handlers.UncompleteNodeHandler)

		// Goal Mapper — Comments
		trustedRoutes.POST("/goals/nodes/:node_id/comments", handlers.CreateCommentHandler)
		trustedRoutes.GET("/goals/nodes/:node_id/comments", handlers.ListCommentsHandler)
		trustedRoutes.PATCH("/goals/comments/:id", handlers.UpdateCommentHandler)
		trustedRoutes.DELETE("/goals/comments/:id", handlers.DeleteCommentHandler)

		// Pappas armhävningar — 60-day pushup challenge. Robert's own private
		// log (#4203/#4194): the operator ruled it single-user, so the routes
		// are gated to the robert account plus Admin (same gate as Jobs/#4196).
		// The single Date-keyed challenge log stays as-is — no schema change; the
		// gate makes the existing log private to robert rather than all-Trusted.
		trustedRoutes.GET("/pushups/entries", handlers.ValidateUserOrAdmin(pushupsOwnerUsername), handlers.GetPushupEntriesHandler)
		trustedRoutes.PATCH("/pushups/entries/:date", handlers.ValidateUserOrAdmin(pushupsOwnerUsername), handlers.PatchPushupEntryHandler)
		trustedRoutes.GET("/pushups/milestones", handlers.ValidateUserOrAdmin(pushupsOwnerUsername), handlers.GetPushupMilestonesHandler)

		// Jobs overview (proxied to aspirant-browser /api/jobs* — the
		// deduplicated Berlin part-time English-job feed). Owned by vinoly
		// (#4196/#4194): a single-user Member app, so it is gated to the vinoly
		// account plus Admin, not all-Trusted. The shared feed + global
		// hide/save state stay as-is (operator ruled option B — single-tenant
		// gate, no per-user interaction model).
		trustedRoutes.Any("/jobs", handlers.ValidateUserOrAdmin(jobsOwnerUsername), handlers.JobsProxyHandler)
		trustedRoutes.Any("/jobs/*path", handlers.ValidateUserOrAdmin(jobsOwnerUsername), handlers.JobsProxyHandler)
	}

	// Admin-specific routes
	adminRoutes := router.Group("/")
	adminRoutes.Use(authMiddleware)
	{
		adminRoutes.Use(handlers.RequireTier(handlers.TierAdmin))
		adminRoutes.GET("/assets", handlers.ListAssetsHandler)
		adminRoutes.POST("/assets/upload", handlers.UploadImageHandler)
		adminRoutes.DELETE("/assets", handlers.DeleteAssetHandler)
		adminRoutes.GET("/data_models/roles", handlers.GetAllRolesHandler)
		// Reading the full user roster / a single account moved up from the
		// viewer tier (#5113 D3) — a signed-up viewer must not enumerate users.
		adminRoutes.GET("/data_models/users", handlers.GetAllUsersHandler)
		adminRoutes.GET("/data_models/users/:id", handlers.GetUserHandler)
		adminRoutes.POST("/data_models/users", handlers.CreateUserHandler)
		adminRoutes.PUT("/data_models/users/:id", handlers.UpdateUserHandler)
		adminRoutes.DELETE("/data_models/users/:id", handlers.DeleteUserHandler)
		adminRoutes.GET("/files/usage", handlers.StorageUsageHandler)
		adminRoutes.DELETE("/files/shared/delete/:filename", handlers.DeleteSharedFileHandler)

		// Service health proxies
		adminRoutes.GET("/transcriber/health", handlers.GetTranscriberHealthHandler)
		adminRoutes.GET("/commander/health", handlers.GetCommanderHealthHandler)

		// Voice messages (proxied to transcriber service)
		adminRoutes.GET("/voice-messages", handlers.ListVoiceMessagesHandler)
		adminRoutes.GET("/voice-messages/:id", handlers.GetVoiceMessageHandler)
		adminRoutes.POST("/voice-messages", handlers.UploadVoiceMessageHandler)
		adminRoutes.DELETE("/voice-messages/:id", handlers.DeleteVoiceMessageHandler)
		adminRoutes.GET("/voice-messages/:id/audio", handlers.GetVoiceAudioHandler)

		// Commander (proxied to commander service)
		adminRoutes.GET("/commander/tasks", handlers.ListCommanderTasksHandler)
		adminRoutes.GET("/commander/tasks/:id", handlers.GetCommanderTaskHandler)
		adminRoutes.PATCH("/commander/tasks/:id", handlers.UpdateCommanderTaskHandler)
		adminRoutes.DELETE("/commander/tasks/:id", handlers.DeleteCommanderTaskHandler)
		adminRoutes.POST("/commander/process", handlers.TriggerCommanderProcessHandler)
		adminRoutes.GET("/commander/vocabulary", handlers.GetCommanderVocabularyHandler)

		// Valuation Statement — shared operator defaults (appraiser identity +
		// default likviditet). Admin-only: these are global config writable by
		// any Trusted user before #3182. Moved here from trustedRoutes as the
		// #3096 follow-up (integrity finding on shared config; OWASP A01:2021).
		adminRoutes.PUT("/commander/valuation-statement/operator-defaults", handlers.PutCommanderValuationOperatorDefaultsHandler)

		// Commander notes (proxied to commander service)
		adminRoutes.GET("/commander/notes", handlers.ListCommanderNotesHandler)
		adminRoutes.GET("/commander/notes/:id", handlers.GetCommanderNoteHandler)
		adminRoutes.PATCH("/commander/notes/:id", handlers.UpdateCommanderNoteHandler)
		adminRoutes.DELETE("/commander/notes/:id", handlers.DeleteCommanderNoteHandler)

		// System monitoring (proxied to monitor sidecar + local DB stats)
		adminRoutes.GET("/system/health", handlers.GetMonitorHealthHandler)
		adminRoutes.GET("/system/containers", handlers.GetMonitorContainersHandler)
		adminRoutes.GET("/system/disk", handlers.GetMonitorDiskHandler)
		adminRoutes.GET("/system/db-stats", handlers.GetDBStatsHandler)

		// Advisor (proxied to advisor service)
		adminRoutes.GET("/advisor/health", handlers.GetAdvisorHealthHandler)
		adminRoutes.GET("/advisor/sources", handlers.GetAdvisorSourcesHandler)
		adminRoutes.POST("/advisor/query", handlers.QueryAdvisorHandler)
		adminRoutes.GET("/advisor/documents", handlers.ListAdvisorDocumentsHandler)
		adminRoutes.GET("/advisor/documents/:id", handlers.GetAdvisorDocumentHandler)
		adminRoutes.GET("/advisor/documents/:id/chunks", handlers.GetAdvisorDocumentChunksHandler)
		adminRoutes.POST("/advisor/documents", handlers.UploadAdvisorDocumentHandler)
		adminRoutes.DELETE("/advisor/documents/:id", handlers.DeleteAdvisorDocumentHandler)
		adminRoutes.POST("/advisor/documents/:id/reprocess", handlers.ReprocessAdvisorDocumentHandler)
		adminRoutes.POST("/advisor/laws", handlers.IngestAdvisorLawsHandler)

		// Browser flows (proxied to aspirant-browser JSON API)
		adminRoutes.Any("/browser-flows", handlers.BrowserProxyHandler)
		adminRoutes.Any("/browser-flows/*path", handlers.BrowserProxyHandler)

		// Internal subrequest target for nginx auth_request. Wraps the
		// admin auth+role chain in a 200-vs-401/403 gate so nginx can
		// protect upstreams (e.g. aspirant-browser matrix HTML) without
		// each service re-implementing JWT parsing.
		adminRoutes.GET("/internal/verify-admin", handlers.VerifyAdminHandler)

		// Easter Egg Hunt admin
		adminRoutes.POST("/games/easter-hunt/admin/reset", handlers.PostEasterHuntResetHandler)
		adminRoutes.GET("/games/easter-hunt/admin/reveal", handlers.GetEasterHuntRevealHandler)
	}

	log.Println("Routes registered successfully")
}
