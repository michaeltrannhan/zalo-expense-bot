// Package config loads and validates environment-based configuration.
// Invalid or unsafe values are startup errors; no parser silently falls back.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvPilot       = "pilot"
	EnvProduction  = "production"
)

type Config struct {
	AppEnv      string
	DatabaseURL string

	ZaloBotToken      string
	ZaloWebhookSecret string
	ZaloAPIBase       string

	ListenAddr string
	DataDir    string

	ObjectStoreBackend string
	S3Bucket           string
	S3Prefix           string
	S3Endpoint         string
	S3Region           string

	// OriginalRetentionDays is how long receipt image bytes are kept before
	// the hourly sweeper deletes them. Extracted transactions are retained.
	OriginalRetentionDays int

	ExtractionBackend string
	GeminiAPIKey      string
	GeminiModel       string
	GeminiAPIBase     string

	// PilotAllowlist is required in pilot/production. Empty is open only in
	// explicitly selected development/test environments.
	PilotAllowlist map[string]bool

	ExtractionEnabled        bool
	OutboundEnabled          bool
	MonthlyOCRPageLimit      int64
	PerUserDailyReceiptLimit int64
	ZaloMonthlyMessageLimit  int64

	ReceiptWorkerConcurrency      int
	NotificationWorkerConcurrency int
	QueuePollInterval             time.Duration
	SummarySchedulePollInterval   time.Duration

	LogLevel string
}

// Load reads and strictly validates the process environment.
func Load() (Config, error) {
	extractionEnabled, err := envBool("EXTRACTION_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	outboundEnabled, err := envBool("OUTBOUND_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	ocrLimit, err := envInt("MONTHLY_OCR_PAGE_LIMIT", 80)
	if err != nil {
		return Config{}, err
	}
	receiptLimit, err := envInt("PER_USER_DAILY_RECEIPT_LIMIT", 20)
	if err != nil {
		return Config{}, err
	}
	messageLimit, err := envInt("ZALO_MONTHLY_MESSAGE_LIMIT", 3000)
	if err != nil {
		return Config{}, err
	}
	retentionDays, err := envInt("ORIGINAL_RETENTION_DAYS", 1)
	if err != nil {
		return Config{}, err
	}
	receiptConcurrency, err := envInt("RECEIPT_WORKER_CONCURRENCY", 4)
	if err != nil {
		return Config{}, err
	}
	notificationConcurrency, err := envInt("NOTIFICATION_WORKER_CONCURRENCY", 2)
	if err != nil {
		return Config{}, err
	}
	queuePoll, err := envDur("QUEUE_POLL_INTERVAL", 300*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	summaryPoll, err := envDur("SUMMARY_SCHEDULE_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		AppEnv:                        envStr("APP_ENV", EnvDevelopment),
		DatabaseURL:                   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ZaloBotToken:                  strings.TrimSpace(os.Getenv("ZALO_BOT_TOKEN")),
		ZaloWebhookSecret:             os.Getenv("ZALO_WEBHOOK_SECRET"),
		ZaloAPIBase:                   envStr("ZALO_API_BASE", "https://bot-api.zaloplatforms.com"),
		ListenAddr:                    envStr("LISTEN_ADDR", "127.0.0.1:8080"),
		DataDir:                       envStr("DATA_DIR", "./data"),
		ObjectStoreBackend:            envStr("OBJECTSTORE", "local"),
		S3Bucket:                      strings.TrimSpace(os.Getenv("S3_BUCKET")),
		S3Prefix:                      strings.TrimSpace(os.Getenv("S3_PREFIX")),
		S3Endpoint:                    strings.TrimRight(strings.TrimSpace(os.Getenv("S3_ENDPOINT")), "/"),
		S3Region:                      strings.TrimSpace(os.Getenv("S3_REGION")),
		ExtractionBackend:             envStr("EXTRACTOR", "mock"),
		GeminiAPIKey:                  strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GeminiModel:                   envStr("GEMINI_MODEL", "gemini-3.6-flash"),
		GeminiAPIBase:                 envStr("GEMINI_API_BASE", "https://generativelanguage.googleapis.com"),
		PilotAllowlist:                parseList(os.Getenv("PILOT_ALLOWLIST")),
		ExtractionEnabled:             extractionEnabled,
		OutboundEnabled:               outboundEnabled,
		MonthlyOCRPageLimit:           ocrLimit,
		PerUserDailyReceiptLimit:      receiptLimit,
		ZaloMonthlyMessageLimit:       messageLimit,
		OriginalRetentionDays:         int(retentionDays),
		ReceiptWorkerConcurrency:      int(receiptConcurrency),
		NotificationWorkerConcurrency: int(notificationConcurrency),
		QueuePollInterval:             queuePoll,
		SummarySchedulePollInterval:   summaryPoll,
		LogLevel:                      envStr("LOG_LEVEL", "info"),
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch c.AppEnv {
	case EnvDevelopment, EnvTest, EnvPilot, EnvProduction:
	default:
		return fmt.Errorf("APP_ENV must be development, test, pilot or production, got %q", c.AppEnv)
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if c.ZaloWebhookSecret == "" {
		return fmt.Errorf("ZALO_WEBHOOK_SECRET is required (set a random value even for local dev)")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return fmt.Errorf("LISTEN_ADDR must be host:port: %w", err)
	}
	switch c.ObjectStoreBackend {
	case "local":
		// Leftover S3_ENDPOINT in .env is ignored; files go to DATA_DIR.
	case "s3":
		if c.S3Bucket == "" {
			return fmt.Errorf("S3_BUCKET is required when OBJECTSTORE=s3")
		}
		if c.S3Endpoint != "" {
			if err := requireObjectStoreEndpoint("S3_ENDPOINT", c.S3Endpoint); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("OBJECTSTORE must be local or s3, got %q", c.ObjectStoreBackend)
	}
	switch c.ExtractionBackend {
	case "mock", "textract":
	case "gemini":
		if c.GeminiAPIKey == "" {
			return fmt.Errorf("GEMINI_API_KEY is required when EXTRACTOR=gemini")
		}
		if !validModelID(c.GeminiModel) {
			return fmt.Errorf("GEMINI_MODEL must contain only letters, digits, dot, underscore or hyphen")
		}
	default:
		return fmt.Errorf("EXTRACTOR must be mock, textract or gemini, got %q", c.ExtractionBackend)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
	}
	for name, value := range map[string]int64{
		"MONTHLY_OCR_PAGE_LIMIT":       c.MonthlyOCRPageLimit,
		"PER_USER_DAILY_RECEIPT_LIMIT": c.PerUserDailyReceiptLimit,
		"ZALO_MONTHLY_MESSAGE_LIMIT":   c.ZaloMonthlyMessageLimit,
	} {
		if value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	if c.OriginalRetentionDays < 1 || c.OriginalRetentionDays > 365 {
		return fmt.Errorf("ORIGINAL_RETENTION_DAYS must be between 1 and 365")
	}
	for name, value := range map[string]int{
		"RECEIPT_WORKER_CONCURRENCY":      c.ReceiptWorkerConcurrency,
		"NOTIFICATION_WORKER_CONCURRENCY": c.NotificationWorkerConcurrency,
	} {
		if value < 1 || value > 64 {
			return fmt.Errorf("%s must be between 1 and 64", name)
		}
	}
	if c.QueuePollInterval < 10*time.Millisecond || c.QueuePollInterval > time.Minute {
		return fmt.Errorf("QUEUE_POLL_INTERVAL must be between 10ms and 1m")
	}
	if c.SummarySchedulePollInterval < time.Second || c.SummarySchedulePollInterval > time.Hour {
		return fmt.Errorf("SUMMARY_SCHEDULE_POLL_INTERVAL must be between 1s and 1h")
	}

	controlled := c.AppEnv == EnvPilot || c.AppEnv == EnvProduction
	if controlled {
		if len(c.PilotAllowlist) == 0 {
			return fmt.Errorf("PILOT_ALLOWLIST must not be empty when APP_ENV=%s", c.AppEnv)
		}
		if c.ZaloBotToken == "" {
			return fmt.Errorf("ZALO_BOT_TOKEN is required when APP_ENV=%s", c.AppEnv)
		}
		if len(c.ZaloWebhookSecret) < 16 || c.ZaloWebhookSecret == "dev-secret-change-me" {
			return fmt.Errorf("ZALO_WEBHOOK_SECRET must be a non-placeholder value of at least 16 characters when APP_ENV=%s", c.AppEnv)
		}
		if c.MonthlyOCRPageLimit == 0 || c.PerUserDailyReceiptLimit == 0 || c.ZaloMonthlyMessageLimit == 0 {
			return fmt.Errorf("all quota limits must be positive when APP_ENV=%s", c.AppEnv)
		}
		if err := requireHTTPS("ZALO_API_BASE", c.ZaloAPIBase); err != nil {
			return err
		}
		if c.ExtractionBackend == "gemini" {
			if err := requireHTTPS("GEMINI_API_BASE", c.GeminiAPIBase); err != nil {
				return err
			}
		}
	}
	if c.AppEnv == EnvProduction {
		if c.ExtractionBackend == "mock" {
			return fmt.Errorf("EXTRACTOR must not be mock when APP_ENV=production")
		}
	}
	return nil
}

// MessagingMode reports whether real Zalo calls are configured. Without a
// bot token development/test use the logging provider and never dial out.
func (c Config) MessagingMode() string {
	if c.ZaloBotToken == "" {
		return "log"
	}
	return "zalo"
}

// Allowlisted reports whether a sender may use the bot. Load guarantees an
// empty list exists only in explicitly open development/test environments.
func (c Config) Allowlisted(senderID string) bool {
	if len(c.PilotAllowlist) == 0 {
		return c.AppEnv == "" || c.AppEnv == EnvDevelopment || c.AppEnv == EnvTest
	}
	return c.PilotAllowlist[senderID]
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return b, nil
}

func envInt(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func envDur(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return d, nil
}

func parseList(s string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out[item] = true
		}
	}
	return out
}

func requireHTTPS(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute https URL", name)
	}
	return nil
}

// requireObjectStoreEndpoint accepts HTTPS anywhere, or HTTP on loopback
// (local MinIO). Public HTTP endpoints are rejected.
func requireObjectStoreEndpoint(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", name)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			return nil
		}
		return fmt.Errorf("%s http is allowed only on loopback (local MinIO)", name)
	default:
		return fmt.Errorf("%s must be an absolute http(s) URL", name)
	}
}

// OriginalRetention is the image keep window and the policy label written
// onto new receipt rows. Zero (tests that skip Load) falls back to 1 day.
func (c Config) OriginalRetention() (keep time.Duration, policy string) {
	days := c.OriginalRetentionDays
	if days < 1 {
		days = 1
	}
	return time.Duration(days) * 24 * time.Hour, fmt.Sprintf("originals_%dd", days)
}

func validModelID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}
