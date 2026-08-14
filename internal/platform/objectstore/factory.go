package objectstore

import (
	"context"

	"zl-expese-bot/internal/config"
)

// NewFromConfig builds the receipt object store: local filesystem by
// default, S3 (or S3-compatible such as Cloudflare R2) when OBJECTSTORE=s3.
func NewFromConfig(ctx context.Context, cfg config.Config) (Store, error) {
	if cfg.ObjectStoreBackend != "s3" {
		return NewLocal(cfg.DataDir)
	}
	return ConnectS3(ctx, cfg.S3Bucket, cfg.S3Prefix, cfg.S3Endpoint, cfg.S3Region)
}
