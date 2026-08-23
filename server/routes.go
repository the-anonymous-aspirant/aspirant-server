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
	router.POST("/games/word_weaver", handlers.GetLongestWordsHandler)
	router.GET("/fetch-object/:etag", handlers.FetchObjectHandler)
	router.GET("/games/scores", handlers.GetGameScoresHandler)
	// Bootstrap route for creating first admin user when no users exist
	router.POST("/bootstrap/admin", handlers.BootstrapUserHandler)

	// Authentication middleware
	authMiddleware := middleware.AuthMiddleware()

	// Routes accessible to logged in users
	authRoutes := router.Group("/")
	authRoutes.Use(authMiddleware)
	{
		authRoutes.GET("/data_models/users/:id", handlers.GetUserHandler)
		authRoutes.GET("/data_models/users", handlers.GetAllUsersHandler)
		authRoutes.POST("/games/scores", handlers.SaveGameScoreHandler)

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
	}

	// Trusted-specific routes (requires Trusted or Admin role)
	trustedRoutes := router.Group("/")
	trustedRoutes.Use(authMiddleware)
	{
		trustedRoutes.Use(handlers.ValidateRole("Trusted", "Admin"))

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

		// Wikipedia (proxied to kiwix-serve)
		trustedRoutes.Any("/wikipedia/*path", handlers.WikipediaProxyHandler)

		// Ludde feeding times
		trustedRoutes.POST("/upload", handlers.UploadImageHandler)
		trustedRoutes.GET("/data_models/ludde_feeding_times", handlers.GetAllFeedingTimesHandler)
		trustedRoutes.GET("/data_models/ludde_feeding_times/:id", handlers.GetFeedingTimeHandler)
		trustedRoutes.POST("/data_models/ludde_feeding_times", handlers.AddFeedingTimeHandler)
		trustedRoutes.DELETE("/data_models/ludde_feeding_times/:id", handlers.DeleteFeedingTimeHandler)

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

		// Pappas armhävningar — 60-day pushup challenge
		trustedRoutes.GET("/pushups/entries", handlers.GetPushupEntriesHandler)
		trustedRoutes.PATCH("/pushups/entries/:date", handlers.PatchPushupEntryHandler)
		trustedRoutes.GET("/pushups/milestones", handlers.GetPushupMilestonesHandler)

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
		adminRoutes.Use(handlers.ValidateRole("Admin"))
		adminRoutes.GET("/assets", handlers.ListAssetsHandler)
		adminRoutes.POST("/assets/upload", handlers.UploadImageHandler)
		adminRoutes.DELETE("/assets", handlers.DeleteAssetHandler)
		adminRoutes.GET("/data_models/roles", handlers.GetAllRolesHandler)
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

		// Finance (proxied to finance service)
		adminRoutes.GET("/finance/health", handlers.GetFinanceHealthHandler)
		adminRoutes.POST("/finance/sources/:bank/upload", handlers.UploadFinanceCSVHandler)
		adminRoutes.GET("/finance/sources", handlers.ListFinanceSourcesHandler)
		adminRoutes.GET("/finance/sources/:bank/schema", handlers.GetFinanceSourceSchemaHandler)
		adminRoutes.GET("/finance/transactions", handlers.ListFinanceTransactionsHandler)
		adminRoutes.GET("/finance/summary/monthly", handlers.GetFinanceMonthlySummaryHandler)
		adminRoutes.GET("/finance/summary/overview", handlers.GetFinanceOverviewHandler)
		adminRoutes.GET("/finance/summary/recurring", handlers.GetFinanceRecurringHandler)
		adminRoutes.GET("/finance/summary/outliers", handlers.GetFinanceOutliersHandler)
		adminRoutes.GET("/finance/categories", handlers.ListFinanceCategoriesHandler)
		adminRoutes.POST("/finance/categories", handlers.CreateFinanceCategoryHandler)
		adminRoutes.DELETE("/finance/categories/:id", handlers.DeleteFinanceCategoryHandler)
		adminRoutes.POST("/finance/re-enrich", handlers.ReEnrichFinanceHandler)
		adminRoutes.GET("/finance/accounts", handlers.ListFinanceAccountsHandler)
		adminRoutes.POST("/finance/import-local", handlers.ImportLocalFinanceHandler)

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
