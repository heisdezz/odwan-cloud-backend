package s3

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
)

type downloadS3 struct {
	*fakeS3
	rangeHeader string
}

func (f *downloadS3) GetObject(ctx context.Context, input *sdk.GetObjectInput, options ...func(*sdk.Options)) (*sdk.GetObjectOutput, error) {
	f.rangeHeader = aws.ToString(input.Range)
	return &sdk.GetObjectOutput{Body: io.NopCloser(bytes.NewReader([]byte("abc")))}, nil
}
func TestS3StreamRangeAndBucket(t *testing.T) {
	u, f, _, _ := setup(t, 0)
	client := &downloadS3{fakeS3: f}
	u.client = client
	reader, err := u.OpenObject(context.Background(), "bucket", "tests/file", &ByteRange{Start: 3, End: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if client.rangeHeader != "bytes=3-5" {
		t.Fatalf("range=%q", client.rangeHeader)
	}
	if _, err := u.OpenObject(context.Background(), "other", "tests/file", nil); err == nil {
		t.Fatal("wrong bucket accepted")
	}
}
