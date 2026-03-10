package main

import (
	"aspirant-online/server"
	"aspirant-online/server/data_functions"
	"log"
	"os"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

var s3Session *session.Session

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting Logger")

	// Try to load .env file, but don't fail if it doesn't exist (for CI/CD)
	err := godotenv.Load(".env")
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	// Print environment variables for debugging (be careful with sensitive data in production)
	log.Printf("DB_HOST: %s", os.Getenv("DB_HOST"))
	log.Printf("DB_NAME: %s", os.Getenv("DB_NAME"))
	log.Printf("DB_USER: %s", os.Getenv("DB_USER"))
	log.Printf("DB_PORT: %s", os.Getenv("DB_PORT"))
	log.Printf("AWS_REGION: %s", os.Getenv("AWS_REGION"))
	log.Printf("S3_BUCKET_NAME: %s", os.Getenv("S3_BUCKET_NAME"))
	// Note: Not printing passwords or access keys for security

	// Set Gin mode based on GIN_MODE environment variable, which we store in the docker-compose for now
	if mode := os.Getenv("GIN_MODE"); mode != "" {
		gin.SetMode(mode)
	} else {
		gin.SetMode(gin.DebugMode)
	}
	log.Printf("Gin mode set to %s, adjust the compose file to enable more logging", gin.Mode())

	// Initialize the database connection (non-fatal: server can start without DB for S3-only mode)
	db, err := server.SetupDBConnection()
	if err != nil {
		log.Printf("WARNING: Database connection failed: %v", err)
		log.Println("WARNING: Server starting without database — DB-dependent routes will not work")
		log.Println("WARNING: S3 assets, health checks, and static content will still be served")
	} else {
		defer db.Close()
		// Set up the database tables (migrations)
		server.AutoMigrate(db)
		log.Println("Database connected and migrated successfully")
	}

	// Initialize S3 session
	s3Session, err = data_functions.InitS3Session()
	if err != nil {
		log.Fatalf("Error initializing S3 session: %v", err)
		return
	}

	// Initialize the Gin engine
	r := gin.New() // Use gin.New() instead of gin.Default() to avoid default middleware

	// Set up middleware
	server.SetupMiddleware(r)

	// Add the database connection we setup into the context of gin (may be nil if DB unavailable)
	r.Use(func(c *gin.Context) {
		c.Set("db", db)
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
