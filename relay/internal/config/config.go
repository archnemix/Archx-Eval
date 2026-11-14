// Package config loads every tunable from the environment. Nothing here reads
// a file: a Hugging Face Space gives us env vars and nothing else.
package config

import (
	"crypto/sha256"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Addr    string
	BankDir string
	DataDir string

	// TrustedProxyHops is how many reverse proxies sit in front of us.
	// Hugging Face Spaces puts one in front, so the default is 1.
	TrustedProxyHops int

	// DrawMax caps how many examples a single caller may receive.
	DrawMax int
	// StaleAfterUses is how many times an example may be served before the
	// bank treats it as stale and stops handing it out.
	StaleAfterUses int

	Window time.Duration // one draw per window, per identity

	SpamWindow time.Duration // "N requests in <SpamWindow>"
	SpamCount  int
	SpamBan    time.Duration

	BurstWindow time.Duration // "N requests in <BurstWindow>"
	BurstCount  int
	BurstBan    time.Duration

	MaxBodyBytes   int64
	RequestTimeout time.Duration

	// AllowLoopback relaxes IP validation for local development.
	AllowLoopback bool

	// SaltSecret keys every HMAC we compute. Rotating it resets every
	// marker hash, so treat it as permanent.
	SaltSecret string

	HFRepo    string
	HFToken   string
	HFBaseURL string
	HFEnabled bool

	// FlushInterval is the maximum time a rejection is allowed to sit in
	// memory before being persisted. Accepted draws always flush inline.
	FlushInterval time.Duration

	ReputationFile string
	LogFormat      string
}

// Load builds a Config from the environment, applying safe defaults.
func Load() Config {
	c := Config{
		Addr:    envStr("ADDR", ":7860"),
		BankDir: envStr("BANK_DIR", "bank"),
		DataDir: envStr("DATA_DIR", "data"),

		TrustedProxyHops: envInt("TRUSTED_PROXY_HOPS", 1),

		DrawMax:        envInt("DRAW_MAX", 75),
		StaleAfterUses: envInt("STALE_AFTER_USES", 3),

		Window: envDur("WINDOW", time.Hour),

		SpamWindow: envDur("SPAM_WINDOW", 60*time.Second),
		SpamCount:  envInt("SPAM_COUNT", 30),
		SpamBan:    envDur("SPAM_BAN", 48*time.Hour),

		BurstWindow: envDur("BURST_WINDOW", 10*time.Second),
		BurstCount:  envInt("BURST_COUNT", 15),
		BurstBan:    envDur("BURST_BAN", 2*time.Hour),

		MaxBodyBytes:   envInt64("MAX_BODY_BYTES", 1<<20),
		RequestTimeout: envDur("REQUEST_TIMEOUT", 15*time.Second),

		AllowLoopback: envBool("ALLOW_LOOPBACK", false),

		SaltSecret: os.Getenv("RELAY_SALT"),

		HFRepo:    envStr("HF_DATASET", "Archnemix/Archx-Eval"),
		HFToken:   os.Getenv("HF_TOKEN"),
		HFBaseURL: envStr("HF_BASE_URL", "https://huggingface.co"),

		FlushInterval: envDur("FLUSH_INTERVAL", 5*time.Second),

		ReputationFile: envStr("REPUTATION_FILE", ""),
		LogFormat:      envStr("LOG_FORMAT", "json"),
	}
	c.HFEnabled = c.HFToken != "" && !envBool("HF_DISABLED", false)
	if c.SaltSecret == "" {
		// Development fallback. Logged loudly at startup by the caller.
		c.SaltSecret = "dev-insecure-salt-change-me"
	}
	if c.DrawMax <= 0 {
		c.DrawMax = 75
	}
	if c.TrustedProxyHops < 0 {
		c.TrustedProxyHops = 0
	}
	return c
}

// Salt returns the 32-byte HMAC key derived from SaltSecret.
func (c Config) Salt() []byte {
	sum := sha256.Sum256([]byte("archx-eval/marker-salt/v1\x00" + c.SaltSecret))
	return sum[:]
}

// SaltIsDefault reports whether the insecure development salt is in use.
func (c Config) SaltIsDefault() bool {
	return c.SaltSecret == "dev-insecure-salt-change-me"
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envDur(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
}
