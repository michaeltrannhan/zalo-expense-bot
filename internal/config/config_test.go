package config

import (
	"strings"
	"testing"
)

var configKeys = []string{
	"APP_ENV", "DATABASE_URL", "ZALO_BOT_TOKEN", "ZALO_WEBHOOK_SECRET",
	"ZALO_API_BASE", "LISTEN_ADDR", "DATA_DIR", "OBJECTSTORE", "S3_BUCKET",
	"S3_PREFIX", "EXTRACTOR", "GEMINI_API_KEY", "GEMINI_MODEL",
	"GEMINI_API_BASE", "PILOT_ALLOWLIST", "EXTRACTION_ENABLED",
	"OUTBOUND_ENABLED", "MONTHLY_OCR_PAGE_LIMIT",
	"PER_USER_DAILY_RECEIPT_LIMIT", "ZALO_MONTHLY_MESSAGE_LIMIT",
	"RECEIPT_WORKER_CONCURRENCY", "NOTIFICATION_WORKER_CONCURRENCY",
	"QUEUE_POLL_INTERVAL", "SUMMARY_SCHEDULE_POLL_INTERVAL", "LOG_LEVEL",
}

func baseEnv(t *testing.T) {
	t.Helper()
	for _, key := range configKeys {
		t.Setenv(key, "")
	}
	t.Setenv("APP_ENV", EnvDevelopment)
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("ZALO_WEBHOOK_SECRET", "dev-secret")
}

func TestLoadDefaultsExternalFeaturesOff(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExtractionEnabled || cfg.OutboundEnabled {
		t.Fatalf("unsafe defaults: extraction=%v outbound=%v", cfg.ExtractionEnabled, cfg.OutboundEnabled)
	}
	if cfg.AppEnv != EnvDevelopment || cfg.MessagingMode() != "log" {
		t.Fatalf("environment/messaging = %s/%s", cfg.AppEnv, cfg.MessagingMode())
	}
	if !cfg.Allowlisted("open-dev-user") {
		t.Fatal("development with an empty allowlist should be explicitly open")
	}
	if cfg.ZaloAPIBase != "https://bot-api.zaloplatforms.com" {
		t.Fatalf("ZaloAPIBase = %q", cfg.ZaloAPIBase)
	}
	if cfg.GeminiModel != "gemini-3.6-flash" {
		t.Fatalf("GeminiModel = %q", cfg.GeminiModel)
	}
}

func TestLoadRejectsInvalidScalarValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"boolean", "OUTBOUND_ENABLED", "sometimes", "must be a boolean"},
		{"integer", "MONTHLY_OCR_PAGE_LIMIT", "many", "must be an integer"},
		{"duration", "QUEUE_POLL_INTERVAL", "quickly", "must be a duration"},
		{"negative quota", "ZALO_MONTHLY_MESSAGE_LIMIT", "-1", "must be non-negative"},
		{"zero concurrency", "RECEIPT_WORKER_CONCURRENCY", "0", "between 1 and 64"},
		{"huge concurrency", "NOTIFICATION_WORKER_CONCURRENCY", "1000", "between 1 and 64"},
		{"fast poll", "QUEUE_POLL_INTERVAL", "1ms", "between 10ms and 1m"},
		{"invalid log", "LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"invalid environment", "APP_ENV", "staging-ish", "APP_ENV"},
		{"invalid listen", "LISTEN_ADDR", "8080", "host:port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsUnsafeGeminiModelID(t *testing.T) {
	for _, model := range []string{"../other-model", "model name", "model/name"} {
		t.Run(model, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("EXTRACTOR", "gemini")
			t.Setenv("GEMINI_API_KEY", "test-key")
			t.Setenv("GEMINI_MODEL", model)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "GEMINI_MODEL") {
				t.Fatalf("Load error = %v", err)
			}
		})
	}
}

func TestPilotFailsClosed(t *testing.T) {
	t.Run("empty allowlist", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("APP_ENV", EnvPilot)
		t.Setenv("ZALO_BOT_TOKEN", "token")
		t.Setenv("ZALO_WEBHOOK_SECRET", "a-real-secret-value")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "PILOT_ALLOWLIST") {
			t.Fatalf("Load error = %v", err)
		}
	})
	t.Run("missing bot token", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("APP_ENV", EnvPilot)
		t.Setenv("PILOT_ALLOWLIST", "relative-1")
		t.Setenv("ZALO_WEBHOOK_SECRET", "a-real-secret-value")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "ZALO_BOT_TOKEN") {
			t.Fatalf("Load error = %v", err)
		}
	})
	t.Run("placeholder secret", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("APP_ENV", EnvPilot)
		t.Setenv("PILOT_ALLOWLIST", "relative-1")
		t.Setenv("ZALO_BOT_TOKEN", "token")
		t.Setenv("ZALO_WEBHOOK_SECRET", "dev-secret-change-me")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "non-placeholder") {
			t.Fatalf("Load error = %v", err)
		}
	})
	t.Run("insecure API endpoint", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("APP_ENV", EnvPilot)
		t.Setenv("PILOT_ALLOWLIST", "relative-1")
		t.Setenv("ZALO_BOT_TOKEN", "token")
		t.Setenv("ZALO_WEBHOOK_SECRET", "a-real-secret-value")
		t.Setenv("ZALO_API_BASE", "http://example.com")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "absolute https") {
			t.Fatalf("Load error = %v", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("APP_ENV", EnvPilot)
		t.Setenv("PILOT_ALLOWLIST", "relative-1, relative-2")
		t.Setenv("ZALO_BOT_TOKEN", "token")
		t.Setenv("ZALO_WEBHOOK_SECRET", "a-real-secret-value")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Allowlisted("relative-1") || cfg.Allowlisted("stranger") {
			t.Fatalf("allowlist not enforced: %#v", cfg.PilotAllowlist)
		}
	})
}

func TestProductionRequiresCloudAndRealExtractor(t *testing.T) {
	baseEnv(t)
	t.Setenv("APP_ENV", EnvProduction)
	t.Setenv("PILOT_ALLOWLIST", "relative-1")
	t.Setenv("ZALO_BOT_TOKEN", "token")
	t.Setenv("ZALO_WEBHOOK_SECRET", "a-real-secret-value")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OBJECTSTORE must be s3") {
		t.Fatalf("Load error = %v", err)
	}

	t.Setenv("OBJECTSTORE", "s3")
	t.Setenv("S3_BUCKET", "private-receipts")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "EXTRACTOR must not be mock") {
		t.Fatalf("Load error = %v", err)
	}
}
