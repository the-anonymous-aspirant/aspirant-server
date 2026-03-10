package data_functions

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"io"
)

// ListObjects lists objects in the specified S3 bucket and prefix
func ListObjects(sess *session.Session, bucket, prefix string) ([]*s3.Object, error) {
	svc := s3.New(sess)
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	}

	result, err := svc.ListObjectsV2(input)
	if err != nil {
		return nil, err
	}

	return result.Contents, nil
}

func UploadFileToS3(sess *session.Session, bucket, key string, body io.Reader) error {
	svc := s3.New(sess)
	_, err := svc.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   aws.ReadSeekCloser(body),
	})
	return err
}
