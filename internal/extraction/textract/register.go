package textract

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awstextract "github.com/aws/aws-sdk-go-v2/service/textract"

	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/extraction"
)

func init() {
	extraction.Register("textract", func(ctx context.Context, _ config.Config) (extraction.Extractor, error) {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("load AWS config: %w", err)
		}
		return New(awstextract.NewFromConfig(awsCfg)), nil
	})
}
