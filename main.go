package main

import (
	"aspirant-online/server"
	"aspirant-online/server/email"
	"aspirant-online/server/middleware"
	"aspirant-online/server/storage"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting Logger")

	// Try to load .env file, but don't fail if it doesn't exist (for CI/CD)
	err := godotenv.Load(".env")
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	// Refuse to serve traffic on empty / placeholder / short JWT_SECRET.
	// system_3 #1374 — the previous insecure fallback let a forged admin
	// token pass AuthMiddleware.
	if err := middleware.LoadJWTSecret(); err != nil {
		log.Fatalf("SECURITY: %v", err)
	}

	// Print environment variables for debugging (be careful with sensitive data in production)
	log.Printf("DB_HOST: %s", os.Getenv("DB_HOST"))
	log.Printf("DB_NAME: %s", os.Getenv("DB_NAME"))
	log.Printf("DB_USER: %s", os.Getenv("DB_USER"))
	log.Printf("DB_PORT: %s", os.Getenv("DB_PORT"))
	log.Printf("ASSET_BASE_PATH: %s", os.Getenv("ASSET_BASE_PATH"))

	// Set Gin mode based on GIN_MODE environment variable, which we store in the docker-compose for now
	if mode := os.Getenv("GIN_MODE"); mode != "" {
		gin.SetMode(mode)
	} else {
		gin.SetMode(gin.DebugMode)
	}
	log.Printf("Gin mode set to %s, adjust the compose file to enable more logging", gin.Mode())

	// Initialize the database connection. Fail fast: a nil DB reaches every
	// handler as a non-nil interface wrapping a typed-nil *gorm.DB, so the
	// db != nil guards pass and the first DB call panics. Exiting non-zero
	// lets Docker's restart-policy retry until postgres is reachable, rather
	// than serving traffic that panics on every DB-touching request.
	db, err := server.SetupDBConnection()
	if err != nil {
		log.Fatalf("FATAL: database connection failed, exiting so the container restarts: %v", err)
	}
	defer db.Close()
	// Set up the database tables (migrations)
	server.AutoMigrate(db)
	log.Println("Database connected and migrated successfully")

	// Build the outbound-mail sender. Fail fast on a half-configured relay:
	// SenderFromEnv returns ErrIncompleteConfig when some SMTP_* variables are
	// set and some are missing, and the alternative to exiting is a service
	// that answers 200 to every sign-up while writing the verification mail to
	// a log nobody reads. That failure is silent and permanent; this one is a
	// container that will not start.
	//
	// With none of them set it returns the development sink, which is the
	// current production state: system_3 #5119 is waiting on the operator to
	// choose a relay (corpus/decisions/2026-09-04-signup-email-provider.md).
	// The startup line below is how that state is visible without reading env.
	mailer, mailerSends, err := email.SenderFromEnv()
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	if mailerSends {
		log.Printf("Email: sending via SMTP relay %s", os.Getenv(email.EnvHost))
	} else {
		log.Println("Email: NO SMTP relay configured — messages are written to this log and not delivered")
	}

	// Initialize local asset storage
	assetPath := os.Getenv("ASSET_BASE_PATH")
	if assetPath == "" {
		assetPath = "/data/assets"
	}
	assets, err := storage.NewLocalStorage(assetPath)
	if err != nil {
		log.Printf("WARNING: Asset storage init failed: %v", err)
		log.Println("WARNING: Asset-dependent routes will not work")
	} else {
		log.Printf("Asset storage initialized at %s (%d files indexed)", assetPath, assets.IndexSize())
	}

	// Initialize the Gin engine
	r := gin.New() // Use gin.New() instead of gin.Default() to avoid default middleware

	// Set up middleware
	server.SetupMiddleware(r)

	// Add the database connection we setup into the context of gin (always non-nil: we fail fast above)
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
		c.Next()
	})

	// Add asset storage to context
	r.Use(func(c *gin.Context) {
		c.Set("storage", assets)
		c.Next()
	})

	// Add the mail sender to context, alongside db and storage. Always
	// non-nil: SenderFromEnv either returns a usable Sender or the process
	// has already exited above.
	r.Use(func(c *gin.Context) {
		c.Set("mailer", mailer)
		c.Next()
	})

	// Set up the server routes
	server.RegisterRoutes(r, db)

	// Start the server
	err = r.Run(":8080")
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
		return
	}
}
