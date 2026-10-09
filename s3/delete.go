package s3

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Versioned buckets retain older versions according to their lifecycle policy.
func (u *Uploader) DeleteObject(ctx context.Context, bucket, key, etag string) error {
	if bucket != u.config.S3.Bucket || key == "" {
		return fmt.Errorf("storage object is not configured")
	}
	client, ok := u.client.(interface {
		DeleteObject(context.Context, *sdk.DeleteObjectInput, ...func(*sdk.Options)) (*sdk.DeleteObjectOutput, error)
	})
	if !ok {
		return fmt.Errorf("storage client cannot delete objects")
	}
	input := &sdk.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if etag != "" {
		input.IfMatch = aws.String(etag)
	}
	_, err := client.DeleteObject(ctx, input)
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
		return nil
	}
	return err
}
