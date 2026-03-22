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
	r.Use(cors.Default())
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

	// Public routes - no authentication required
	router.POST("/login", handlers.LoginHandler)
	router.GET("/login/:username", handlers.LoginUserHandler)
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
	}

	// Trusted-specific routes (requires Trusted or Admin role)
	trustedRoutes := router.Group("/")
	trustedRoutes.Use(authMiddleware)
	{
		trustedRoutes.Use(handlers.ValidateRole("Trusted", "Admin"))

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

		// Wikipedia (proxied to kiwix-serve)
		trustedRoutes.Any("/wikipedia/*path", handlers.WikipediaProxyHandler)

		// Advisor (proxied to advisor service)
		trustedRoutes.GET("/advisor/health", handlers.GetAdvisorHealthHandler)
		trustedRoutes.GET("/advisor/sources", handlers.GetAdvisorSourcesHandler)
		trustedRoutes.POST("/advisor/query", handlers.QueryAdvisorHandler)
		trustedRoutes.GET("/advisor/documents", handlers.ListAdvisorDocumentsHandler)
		trustedRoutes.GET("/advisor/documents/:id", handlers.GetAdvisorDocumentHandler)
		trustedRoutes.GET("/advisor/documents/:id/chunks", handlers.GetAdvisorDocumentChunksHandler)

		// Ludde feeding times
		trustedRoutes.POST("/upload", handlers.UploadImageHandler)
		trustedRoutes.GET("/data_models/ludde_feeding_times", handlers.GetAllFeedingTimesHandler)
		trustedRoutes.GET("/data_models/ludde_feeding_times/:id", handlers.GetFeedingTimeHandler)
		trustedRoutes.POST("/data_models/ludde_feeding_times", handlers.AddFeedingTimeHandler)
		trustedRoutes.DELETE("/data_models/ludde_feeding_times/:id", handlers.DeleteFeedingTimeHandler)
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

		// reMarkable (proxied to remarkable service)
		adminRoutes.GET("/remarkable/health", handlers.GetRemarkableHealthHandler)
		adminRoutes.GET("/remarkable/notebooks", handlers.ListRemarkableNotebooksHandler)
		adminRoutes.GET("/remarkable/notebooks/:id", handlers.GetRemarkableNotebookHandler)
		adminRoutes.GET("/remarkable/notebooks/:id/pages/:page/render", handlers.RenderRemarkablePageHandler)
		adminRoutes.GET("/remarkable/notebooks/:id/export", handlers.ExportRemarkableNotebookHandler)
		adminRoutes.GET("/remarkable/folders", handlers.ListRemarkableFoldersHandler)
		adminRoutes.GET("/remarkable/folders/:id/contents", handlers.GetRemarkableFolderContentsHandler)
		adminRoutes.GET("/remarkable/tree", handlers.GetRemarkableTreeHandler)
		adminRoutes.POST("/remarkable/sync", handlers.SyncRemarkableHandler)
		adminRoutes.GET("/remarkable/sync/status", handlers.GetRemarkableSyncStatusHandler)
		adminRoutes.POST("/remarkable/to-device/upload", handlers.UploadRemarkableToDeviceHandler)
		adminRoutes.GET("/remarkable/to-device/pending", handlers.ListRemarkablePendingHandler)
		adminRoutes.DELETE("/remarkable/to-device/:id", handlers.DeleteRemarkablePendingHandler)

		// Advisor admin (proxied to advisor service)
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
	}

	log.Println("Routes registered successfully")
}
