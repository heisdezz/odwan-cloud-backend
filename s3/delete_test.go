package s3

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"testing"
)

type deletingS3 struct {
	*fakeS3
	input   *sdk.DeleteObjectInput
	failure error
}

func (f *deletingS3) DeleteObject(_ context.Context, input *sdk.DeleteObjectInput, _ ...func(*sdk.Options)) (*sdk.DeleteObjectOutput, error) {
	f.input = input
	return &sdk.DeleteObjectOutput{}, f.failure
}
func TestDeleteProtectsBucketAndETag(t *testing.T) {
	u, f, _, _ := setup(t, 0)
	client := &deletingS3{fakeS3: f}
	u.client = client
	if err := u.DeleteObject(context.Background(), "other", "file", "etag"); err == nil || client.input != nil {
		t.Fatal("wrong bucket accepted")
	}
	if err := u.DeleteObject(context.Background(), "bucket", "file", "etag"); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(client.input.IfMatch) != "etag" {
		t.Fatal("object replacement not protected")
	}
	client.failure = &smithy.GenericAPIError{Code: "NoSuchKey"}
	if err := u.DeleteObject(context.Background(), "bucket", "file", "etag"); err != nil {
		t.Fatal("missing file not idempotent", err)
	}
	client.failure = &smithy.GenericAPIError{Code: "PreconditionFailed"}
	if err := u.DeleteObject(context.Background(), "bucket", "file", "etag"); err == nil {
		t.Fatal("replacement failure swallowed")
	}
}
