package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type fakeS3 struct {
	mu        sync.Mutex
	objectKey string
	hasObject bool
	api
	parts          map[int32][]byte
	calls          map[int32]int
	metadata       map[string]string
	completed      []byte
	failPart       int32
	loseCompletion bool
	exists         bool
	creates        int
	aborted        bool
}

func (f *fakeS3) CreateMultipartUpload(ctx context.Context, in *sdk.CreateMultipartUploadInput, opts ...func(*sdk.Options)) (*sdk.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.exists = true
	f.metadata = in.Metadata
	return &sdk.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}
func (f *fakeS3) ListParts(ctx context.Context, in *sdk.ListPartsInput, opts ...func(*sdk.Options)) (*sdk.ListPartsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return nil, &smithy.GenericAPIError{Code: "NoSuchUpload"}
	}
	marker, _ := strconv.Atoi(aws.ToString(in.PartNumberMarker))
	nums := []int{}
	for n := range f.parts {
		if int(n) > marker {
			nums = append(nums, int(n))
		}
	}
	sort.Ints(nums)
	out := &sdk.ListPartsOutput{}
	if len(nums) > 0 {
		n := int32(nums[0])
		out.Parts = []types.Part{{PartNumber: aws.Int32(n), Size: aws.Int64(int64(len(f.parts[n]))), ETag: aws.String("etag-" + strconv.Itoa(int(n)))}}
		out.IsTruncated = aws.Bool(len(nums) > 1)
		out.NextPartNumberMarker = aws.String(strconv.Itoa(int(n)))
	}
	return out, nil
}
func (f *fakeS3) UploadPart(ctx context.Context, in *sdk.UploadPartInput, opts ...func(*sdk.Options)) (*sdk.UploadPartOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := aws.ToInt32(in.PartNumber)
	f.calls[n]++
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.parts[n] = data
	// The server accepted the part, but its response was lost.
	if n == f.failPart {
		f.failPart = 0
		return nil, errors.New("connection lost")
	}
	return &sdk.UploadPartOutput{ETag: aws.String("etag-" + strconv.Itoa(int(n)))}, nil
}
func (f *fakeS3) CompleteMultipartUpload(ctx context.Context, in *sdk.CompleteMultipartUploadInput, opts ...func(*sdk.Options)) (*sdk.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = nil
	for _, p := range in.MultipartUpload.Parts {
		f.completed = append(f.completed, f.parts[aws.ToInt32(p.PartNumber)]...)
	}
	f.exists = false
	f.hasObject = true
	f.objectKey = aws.ToString(in.Key)
	if f.loseCompletion {
		f.loseCompletion = false
		return nil, errors.New("completion response lost")
	}
	return &sdk.CompleteMultipartUploadOutput{ETag: aws.String("complete")}, nil
}
func (f *fakeS3) HeadObject(ctx context.Context, in *sdk.HeadObjectInput, opts ...func(*sdk.Options)) (*sdk.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasObject || f.objectKey != aws.ToString(in.Key) {
		return nil, &smithy.GenericAPIError{Code: "NotFound"}
	}
	return &sdk.HeadObjectOutput{Metadata: f.metadata, ContentLength: aws.Int64(int64(len(f.completed))), ETag: aws.String("complete")}, nil
}
func (f *fakeS3) AbortMultipartUpload(ctx context.Context, in *sdk.AbortMultipartUploadInput, opts ...func(*sdk.Options)) (*sdk.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = true
	f.exists = false
	return &sdk.AbortMultipartUploadOutput{}, nil
}
func (f *fakeS3) PutObject(ctx context.Context, in *sdk.PutObjectInput, opts ...func(*sdk.Options)) (*sdk.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := io.ReadAll(in.Body)
	f.completed = data
	f.hasObject = true
	f.objectKey = aws.ToString(in.Key)
	f.metadata = in.Metadata
	return &sdk.PutObjectOutput{ETag: aws.String("empty")}, err
}
func setup(t *testing.T, size int) (*Uploader, *fakeS3, string, []byte) {
	t.Helper()
	c := Config{StoragePath: t.TempDir(), S3: S3Config{Endpoint: "https://example.com", Region: "region", Bucket: "bucket", PartSize: 5 << 20, Concurrency: 1, AccessKeyID: "test", SecretAccessKey: "test"}}
	u, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeS3{parts: map[int32][]byte{}, calls: map[int32]int{}}
	u.client = fake
	data := bytes.Repeat([]byte("x"), size)
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return u, fake, path, data
}
func TestResumeAfterLostPartResponse(t *testing.T) {
	u, fake, path, data := setup(t, (10<<20)+17)
	fake.failPart = 2
	if _, err := u.UploadFile(context.Background(), path, "test", nil); err == nil {
		t.Fatal("expected interruption")
	}
	// A fresh uploader represents a process restart with only disk state retained.
	resumed := &Uploader{client: fake, config: u.config}
	var progress Progress
	if _, err := resumed.UploadFile(context.Background(), path, "test", func(p Progress) { progress = p }); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 1 || fake.calls[1] != 1 || fake.calls[2] != 1 || fake.calls[3] != 1 {
		t.Fatalf("parts were uploaded again: %v", fake.calls)
	}
	if !bytes.Equal(fake.completed, data) {
		t.Fatal("uploaded bytes differ")
	}
	if progress.Percentage != 100 || progress.BytesWritten != int64(len(data)) {
		t.Fatalf("incorrect progress: %+v", progress)
	}
	states, _ := filepath.Glob(filepath.Join(u.config.StoragePath, ".s3-uploads", "*.json"))
	if len(states) != 0 {
		t.Fatal("checkpoint retained after completion")
	}
}
func TestRecoverLostCompletionResponse(t *testing.T) {
	u, fake, path, _ := setup(t, 64)
	fake.loseCompletion = true
	if _, err := u.UploadFile(context.Background(), path, "test", nil); err == nil {
		t.Fatal("expected interruption")
	}
	if _, err := u.UploadFile(context.Background(), path, "test", nil); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 1 || fake.calls[1] != 1 {
		t.Fatal("completed object uploaded twice")
	}
}
func TestChangedSourceAndAbort(t *testing.T) {
	u, fake, path, _ := setup(t, 64)
	fake.failPart = 1
	if _, err := u.UploadFile(context.Background(), path, "test", nil); err == nil {
		t.Fatal("expected interruption")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), 64), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := u.UploadFile(context.Background(), path, "test", nil); err == nil {
		t.Fatal("accepted modified source")
	}
	if err := u.Abort(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if !fake.aborted {
		t.Fatal("remote upload not aborted")
	}
	if err := u.Abort(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
}
func TestUploadLockAndCancellation(t *testing.T) {
	u, _, path, _ := setup(t, 64)
	_, unlock, err := u.lock("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.UploadFile(context.Background(), path, "test", nil); err == nil {
		t.Fatal("concurrent upload allowed")
	}
	unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.UploadFile(ctx, path, "test", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}
func TestEmptyFile(t *testing.T) {
	u, fake, path, _ := setup(t, 0)
	if _, err := u.UploadFile(context.Background(), path, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 0 || len(fake.completed) != 0 {
		t.Fatal("empty file used multipart")
	}
}

type parallelS3 struct {
	*fakeS3
	mu              sync.Mutex
	active, maximum int
	gate            chan struct{}
}

func (f *parallelS3) UploadPart(ctx context.Context, in *sdk.UploadPartInput, opts ...func(*sdk.Options)) (*sdk.UploadPartOutput, error) {
	f.mu.Lock()
	f.active++
	f.maximum = max(f.maximum, f.active)
	if f.active == 4 && f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
	gate := f.gate
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeS3.UploadPart(ctx, in, opts...)
}
func TestParallelUploadPreservesPartOrder(t *testing.T) {
	u, fake, path, data := setup(t, 30<<20)
	u.config.S3.Concurrency = 4
	client := &parallelS3{fakeS3: fake, gate: make(chan struct{})}
	u.client = client
	for i := range data {
		data[i] = byte(i / (5 << 20))
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var last Progress
	if _, err := u.UploadFile(ctx, path, "parallel", func(p Progress) {
		if p.BytesWritten < last.BytesWritten {
			t.Error("progress went backwards")
		}
		last = p
	}); err != nil {
		t.Fatal(err)
	}
	if client.maximum != 4 {
		t.Fatalf("expected four concurrent parts, got %d", client.maximum)
	}
	if !bytes.Equal(fake.completed, data) {
		t.Fatal("parts completed in the wrong order")
	}
}
func TestUniqueUploadAndRestart(t *testing.T) {
	u, fake, path, _ := setup(t, 64)
	result, err := u.UploadFileUnique(context.Background(), path, "first", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicate || len(result.Hash) != 64 {
		t.Fatal("missing generated SHA-256")
	}
	restarted := &Uploader{client: fake, config: u.config}
	duplicate, err := restarted.UploadFileUnique(context.Background(), path, "different-key", result.Hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.Key != "first" || fake.creates != 1 || fake.calls[1] != 1 {
		t.Fatal("duplicate content sent again")
	}
	// Missing remote content invalidates the local index and allows uploading again.
	fake.hasObject = false
	fake.parts = map[int32][]byte{}
	if _, err := restarted.UploadFileUnique(context.Background(), path, "replacement", result.Hash, nil); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 2 {
		t.Fatal("stale hash index prevented upload")
	}
}
func TestUniqueUploadRejectsBadHashes(t *testing.T) {
	u, fake, path, _ := setup(t, 64)
	for _, tc := range []struct {
		hash string
		want error
	}{{"invalid", ErrInvalidHash}, {string(bytes.Repeat([]byte("0"), 64)), ErrHashMismatch}} {
		if _, err := u.UploadFileUnique(context.Background(), path, "test", tc.hash, nil); !errors.Is(err, tc.want) {
			t.Fatalf("expected %v, got %v", tc.want, err)
		}
	}
	if fake.creates != 0 {
		t.Fatal("bad hash uploaded")
	}
}
func TestUniqueEmptyFile(t *testing.T) {
	u, fake, path, _ := setup(t, 0)
	first, err := u.UploadFileUnique(context.Background(), path, "empty", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := u.UploadFileUnique(context.Background(), path, "another", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || first.Hash != duplicate.Hash || fake.creates != 0 {
		t.Fatal("empty file not deduplicated")
	}
}

func TestUniqueUploadRecoversCompletionAndClearsState(t *testing.T) {
	u, fake, path, _ := setup(t, 64)
	fake.loseCompletion = true
	if _, err := u.UploadFileUnique(context.Background(), path, "test", "", nil); err == nil {
		t.Fatal("expected lost completion response")
	}
	result, err := u.UploadFileUnique(context.Background(), path, "test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Duplicate || fake.creates != 1 {
		t.Fatal("completed content uploaded twice")
	}
	states, _ := filepath.Glob(filepath.Join(u.config.StoragePath, ".s3-uploads", "*.json"))
	if len(states) != 0 {
		t.Fatal("completed checkpoint retained")
	}
}

func TestParallelFailureRemainsResumable(t *testing.T) {
	u, fake, path, data := setup(t, 30<<20)
	u.config.S3.Concurrency = 4
	fake.failPart = 2
	if _, err := u.UploadFile(context.Background(), path, "parallel-resume", nil); err == nil {
		t.Fatal("expected interruption")
	}
	restarted := &Uploader{client: fake, config: u.config}
	if _, err := restarted.UploadFile(context.Background(), path, "parallel-resume", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.completed, data) || fake.creates != 1 {
		t.Fatal("parallel resume did not preserve upload")
	}
	for n, count := range fake.calls {
		if count != 1 {
			t.Fatalf("accepted part %d sent %d times", n, count)
		}
	}
}

type deniedHeadS3 struct{ *fakeS3 }

func (f *deniedHeadS3) HeadObject(context.Context, *sdk.HeadObjectInput, ...func(*sdk.Options)) (*sdk.HeadObjectOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "AccessDenied"}
}
func TestUniqueUploadDoesNotIgnoreHeadErrors(t *testing.T) {
	u, fake, path, _ := setup(t, 64)
	u.client = &deniedHeadS3{fake}
	if _, err := u.UploadFileUnique(context.Background(), path, "test", "", nil); !apiCode(err, "AccessDenied") {
		t.Fatalf("metadata error ignored: %v", err)
	}
	if fake.creates != 0 {
		t.Fatal("uploaded despite failed duplicate check")
	}
}
