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

	// SyncInterval is the batching cadence for dataset commits. The dataset is
	// a git repo: writes are sequential, so they are coalesced rather than
	// issued per request.
	SyncInterval time.Duration
	SyncMaxFiles int
	// RestoreOnBoot pulls the relay's own state back from the dataset before
	// serving, so a sleeping Space resumes with its memory intact.
	RestoreOnBoot bool
	// RestorePrefixes lists the relay-owned dataset subtrees restored at boot.
	// The bank is deliberately excluded from this list because it is read from
	// the dataset on a separate path (BankFromDataset) and never published.
	RestorePrefixes []string

	// BankFromDataset pulls the question bank out of the private dataset at
	// boot. The bank is the answer key and must never be committed to this
	// public repository or uploaded into a public Space, and a Space's disk is
	// wiped on restart, so the private dataset is its only durable home.
	BankFromDataset bool

	// FlushInterval is the maximum time a rejection is allowed to sit in
	// memory before being persisted. Accepted draws always flush inline.
	// RateDir holds the durable sliding-window log. It lives outside DataDir's
	// mirrored tree because it is operational state, not identity data.
	RateDir string

	AccessToken string

	// MetricsToken gates /metrics; empty means the endpoint is disabled.
	MetricsToken string

	// Network caps exist so one subnet cannot exhaust the whole bank.
	NetWindow    time.Duration
	NetMaxDraws  int
	GlobalWindow time.Duration
	GlobalMax    int

	FlushInterval time.Duration

	ReputationFile string
	LogFormat      string
}

// Load builds a Config from the environment, applying safe defaults.
func Load() Config {
	c := Config{
		Addr:    envStr("ADDR", ":7860"),
		BankDir: envStr("BANK_DIR", "bank"),
		// Default on: the bank is useless if the Space starts without it.
		BankFromDataset: envBool("BANK_FROM_DATASET", true),
		DataDir:         envStr("DATA_DIR", "data"),

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

		HFRepo: envStr("HF_DATASET", "Archnemix/Archx-Eval"),
		// HF_DATA_TOKEN is the secret the Space carries for dataset access. It is
		// created by the DATASET owner (Archnemix) and pasted into the Space's
		// settings by hand; the Space's own account is never added as a
		// collaborator and holds no token that can write this dataset.
		//
		// HF_TOKEN is still honoured as a fallback so existing deployments and
		// local test scripts keep working, but the Space must use HF_DATA_TOKEN.
		HFToken:   firstNonEmpty(os.Getenv("HF_DATA_TOKEN"), os.Getenv("HF_TOKEN")),
		HFBaseURL: envStr("HF_BASE_URL", "https://huggingface.co"),

		SyncInterval:  envDur("SYNC_INTERVAL", 30*time.Second),
		SyncMaxFiles:  envInt("SYNC_MAX_FILES", 100),
		RestoreOnBoot: envBool("RESTORE_ON_BOOT", true),
		// Raw IPs and machine identifiers are never stored at all, so there is
		// no "raw" subtree to restore or publish.
		RestorePrefixes: envList("RESTORE_PREFIXES",
			[]string{"identities", "markers", "events", "results"}),

		RateDir:     envStr("RATE_DIR", "rate"),
		AccessToken: os.Getenv("ACCESS_TOKEN"),

		MetricsToken: os.Getenv("METRICS_TOKEN"),

		NetWindow:    envDur("NET_WINDOW", time.Hour),
		NetMaxDraws:  envInt("NET_MAX_DRAWS", 60),
		GlobalWindow: envDur("GLOBAL_WINDOW", time.Hour),
		GlobalMax:    envInt("GLOBAL_MAX_DRAWS", 4000),

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

func envList(key string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// firstNonEmpty returns the first non-empty value, so one config field can be
// fed by a preferred environment variable and a legacy fallback.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
