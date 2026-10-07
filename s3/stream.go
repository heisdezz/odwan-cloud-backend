package s3

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
)

// ByteRange uses inclusive offsets into a logical object.
type ByteRange struct{ Start, End int64 }
type StreamInfo struct {
	Size              int64
	ContentType, ETag string
	Modified          time.Time
}

func (u *Uploader) StatObject(ctx context.Context, bucket, key string) (StreamInfo, error) {
	if bucket != u.config.S3.Bucket {
		return StreamInfo{}, fmt.Errorf("storage bucket is not configured")
	}
	output, err := u.client.HeadObject(ctx, &sdk.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return StreamInfo{}, err
	}
	return StreamInfo{Size: aws.ToInt64(output.ContentLength), ContentType: aws.ToString(output.ContentType), ETag: aws.ToString(output.ETag), Modified: aws.ToTime(output.LastModified)}, nil
}

func (u *Uploader) OpenObject(ctx context.Context, bucket, key string, byteRange *ByteRange) (io.ReadCloser, error) {
	if bucket != u.config.S3.Bucket {
		return nil, fmt.Errorf("storage bucket is not configured")
	}
	client, ok := u.client.(interface {
		GetObject(context.Context, *sdk.GetObjectInput, ...func(*sdk.Options)) (*sdk.GetObjectOutput, error)
	})
	if !ok {
		return nil, fmt.Errorf("storage client cannot download objects")
	}
	input := &sdk.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if byteRange != nil {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-%d", byteRange.Start, byteRange.End))
	}
	output, err := client.GetObject(ctx, input)
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}
