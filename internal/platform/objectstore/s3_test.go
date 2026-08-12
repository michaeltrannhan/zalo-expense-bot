package objectstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"zl-expese-bot/internal/domain"
)

type fakeS3 struct {
	objects map[string][]byte
	sse     map[string]s3types.ServerSideEncryption
	err     error
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, sse: map[string]s3types.ServerSideEncryption{}}
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[*in.Key] = data
	f.sse[*in.Key] = in.ServerSideEncryption
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(data)))}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if _, ok := f.objects[*in.Key]; !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	}
	delete(f.objects, *in.Key)
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3PutOpenDeleteRoundTrip(t *testing.T) {
	fake := newFakeS3()
	st, err := NewS3(fake, "receipts-bucket", "dev")
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	ctx := context.Background()

	stored, err := st.Put(ctx, "receipts/u1/r1", strings.NewReader("fake-image-bytes"), "image/jpeg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored.ByteSize != int64(len("fake-image-bytes")) || stored.SHA256 == "" {
		t.Errorf("stored metadata wrong: %+v", stored)
	}
	if fake.sse["dev/receipts/u1/r1"] != s3types.ServerSideEncryptionAes256 {
		t.Errorf("Put missing SSE-AES256")
	}
	if _, ok := fake.objects["dev/receipts/u1/r1"]; !ok {
		t.Fatalf("object stored under wrong key")
	}

	body, err := st.Open(ctx, "receipts/u1/r1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if string(data) != "fake-image-bytes" {
		t.Errorf("Open body = %q", data)
	}

	if err := st.Delete(ctx, "receipts/u1/r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Open(ctx, "receipts/u1/r1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open after Delete = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, "receipts/u1/r1"); err != nil {
		t.Errorf("Delete missing object must be idempotent, got %v", err)
	}
}

func TestS3KeyValidation(t *testing.T) {
	st, err := NewS3(newFakeS3(), "b", "")
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if _, err := st.Put(context.Background(), "../escape", strings.NewReader("x"), "text/plain"); err == nil {
		t.Errorf("traversal key must be rejected")
	}
}

func TestClassifyS3(t *testing.T) {
	cases := []struct {
		code string
		want domain.Code
	}{
		{"AccessDenied", domain.CodeForbidden},
		{"SlowDown", domain.CodeTransient},
		{"InternalError", domain.CodeTransient},
		{"SomethingNew", domain.CodeTransient},
	}
	for _, c := range cases {
		err := classifyS3("test", &smithy.GenericAPIError{Code: c.code, Message: "x"})
		if got := domain.CodeOf(err); got != c.want {
			t.Errorf("%s: got %s, want %s", c.code, got, c.want)
		}
	}
}
