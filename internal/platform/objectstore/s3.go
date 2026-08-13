package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"zl-expese-bot/internal/domain"
)

// S3API is the slice of the S3 client the store needs (fakeable in tests).
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3 implements Store over Amazon S3 or an S3-compatible API (Cloudflare
// R2, MinIO). AWS buckets get SSE-AES256; compatible endpoints skip that
// header because they encrypt at rest themselves and often reject it.
// The bucket must block all public access; presigned access is a
// post-MVP decision (§11.3).
type S3 struct {
	client     S3API
	bucket     string
	prefix     string
	disableSSE bool
}

var _ Store = (*S3)(nil)

// NewS3 wires the adapter; prefix (usually blank locally) namespaces keys.
func NewS3(client S3API, bucket, prefix string) (*S3, error) {
	if client == nil {
		return nil, fmt.Errorf("objectstore: S3 client is required")
	}
	if bucket == "" {
		return nil, fmt.Errorf("objectstore: S3 bucket is required")
	}
	return &S3{client: client, bucket: bucket, prefix: strings.Trim(prefix, "/")}, nil
}

// ConnectS3 builds an S3 (or S3-compatible) store from process env.
// endpoint, when set, is the HTTPS API base (for example Cloudflare R2
// https://<accountid>.r2.cloudflarestorage.com). Credentials still come
// from the AWS SDK chain (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY).
func ConnectS3(ctx context.Context, bucket, prefix, endpoint, region string) (*S3, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if region == "" && endpoint != "" {
		region = "auto"
	}
	var loadOpts []func(*awsconfig.LoadOptions) error
	if region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load S3 config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
	st, err := NewS3(client, bucket, prefix)
	if err != nil {
		return nil, err
	}
	st.disableSSE = endpoint != ""
	return st, nil
}

func (s *S3) fullKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

// Put uploads the object with SSE and returns the locally computed sha256.
// Receipt images arrive pre-validated and capped (≤ 10 MiB by the receipt
// pipeline), so buffering for hashing stays bounded.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, contentType string) (Stored, error) {
	if err := validateKey(key); err != nil {
		return Stored{}, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return Stored{}, domain.E(domain.CodeTransient, "read object bytes", err)
	}
	sum := sha256.Sum256(data)
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.fullKey(key)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	}
	if !s.disableSSE {
		in.ServerSideEncryption = s3types.ServerSideEncryptionAes256
	}
	_, err = s.client.PutObject(ctx, in)
	if err != nil {
		return Stored{}, classifyS3("put object", err)
	}
	return Stored{
		Key:         key,
		SHA256:      hex.EncodeToString(sum[:]),
		ByteSize:    int64(len(data)),
		ContentType: contentType,
	}, nil
}

// Open streams the object body; the caller closes it.
func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, classifyS3("get object", err)
	}
	return out.Body, nil
}

// Delete removes the object; a missing object is success (idempotent).
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil
		}
		return classifyS3("delete object", err)
	}
	return nil
}

// isS3NotFound reports whether the service said the object does not exist.
func isS3NotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return true
		}
	}
	return false
}

// classifyS3 maps service failures onto domain retry classes: access
// problems are permanent and loud; throttling and 5xx are transient.
func classifyS3(op string, err error) error {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "AccessDenied", "AllAccessDisabled", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return domain.E(domain.CodeForbidden, "s3 "+op+": access denied", err)
		case "SlowDown", "ServiceUnavailable", "InternalError", "RequestTimeout":
			return domain.E(domain.CodeTransient, "s3 "+op, err)
		}
	}
	return domain.E(domain.CodeTransient, "s3 "+op, err)
}
