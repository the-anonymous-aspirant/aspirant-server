package data_functions

import (
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/joho/godotenv"
)

func TestMain(m *testing.M) {
	// Load environment variables from .env file (optional — CI won't have one)
	err := godotenv.Load("../../.env")
	if err != nil {
		log.Println("Warning: .env file not found, using environment variables as-is")
	}

	// Run tests
	os.Exit(m.Run())
}

func TestGetGitCommit(t *testing.T) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	commit := GetGitCommit()
	if commit == "" {
		t.Errorf("GetGitCommit() returned empty string")
	}
	// When built without -ldflags, returns "unknown"; when built with, returns a SHA
	t.Logf("GitCommit: %s", commit)
}

func TestInitS3Session(t *testing.T) {
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")

	log.Printf("Loaded AWS Access Key ID: %s", accessKeyID)
	sess, err := InitS3Session()
	if err != nil {
		t.Fatalf("InitS3Session() failed: %v", err)
	}
	if sess == nil {
		t.Fatalf("InitS3Session() returned nil session")
	}
}

func TestFetchEntityFromS3(t *testing.T) {
	bucket := os.Getenv("S3_BUCKET_NAME")
	etag := "\"0b04a90f672c2aadd9117c3c3d0b50b7\"" // ETag should be wrapped in double quotes

	sess, err := InitS3Session()
	if err != nil {
		t.Fatalf("Failed to initialize S3 session: %v", err)
	}

	//log.Printf("Looking for object with ETag: %s in bucket: %s", etag, bucket)
	key, err := FindKeyByETag(sess, bucket, etag)

	if err != nil {
		t.Fatalf("Failed to find object by ETag: %v", err)
	}

	if key == "" {
		t.Fatalf("Object not found")
	}

	objectData, err := FetchFileFromS3(sess, bucket, key)
	if err != nil {
		t.Fatalf("Failed to fetch object from S3: %v", err)
	}

	fmt.Println("Fetched data:", string(objectData))
}
