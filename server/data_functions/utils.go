package data_functions

import (
	"io/ioutil"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// GitCommit is set at build time via -ldflags
var GitCommit = "unknown"

func GetGitCommit() string {
	return GitCommit
}

func InitS3Session() (*session.Session, error) {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "eu-north-1" // Default to eu-north-1 if not set
	}

	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	var sess *session.Session
	var err error

	if accessKeyID != "" && secretAccessKey != "" {
		sess, err = session.NewSession(&aws.Config{
			Region:      aws.String(region),
			Credentials: credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""),
		})
	} else {
		profile := "aspirant"
		sess, err = session.NewSessionWithOptions(session.Options{
			Config:  aws.Config{Region: aws.String(region)},
			Profile: profile,
		})
	}

	if err != nil {
		return nil, err
	}
	return sess, nil
}

func FetchFileFromS3(sess *session.Session, bucket, key string) ([]byte, error) {
	svc := s3.New(sess)
	result, err := svc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	return ioutil.ReadAll(result.Body)
}

func FindKeyByETag(sess *session.Session, bucket, etag string) (string, error) {
	svc := s3.New(sess)
	var foundKey string
	err := svc.ListObjectsV2Pages(&s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	}, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, obj := range page.Contents {
			//log.Printf("Checking object: %s with ETag: %s", *obj.Key, *obj.ETag)
			if *obj.ETag == etag {
				foundKey = *obj.Key
				return false
			}
		}
		return !lastPage
	})
	if err != nil {
		return "", err
	}
	return foundKey, nil
}
