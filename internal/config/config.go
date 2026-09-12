// Package config loads .pgbot.toml — the committed-to-the-repo file that lets a
// team override thresholds, remap severities, and suppress specific findings so
// noise never trains people to ignore the severity column (B2). It is
// deliberately conservative: it refuses to read anything credential-shaped, and
// a typo becomes a loud warning rather than a silently-ignored rule.
package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pgrundev/pgbot/internal/findings"
)

// CurrentSchema is the config schema this binary understands.
const CurrentSchema = 1

// expiryLayout is the date format for an ignore rule's `expires`.
const expiryLayout = "2006-01-02"

// forbiddenKeys are credential-shaped keys pgbot REFUSES to read from a config
// file. The whole point of the file is that it gets committed to a repository;
// the failure mode is a DSN or password ending up in git history. Matched
// case-insensitively on the last element of any key path, so [x] dsn = "…" and a
// nested password are both caught.
var forbiddenKeys = map[string]bool{
	"dsn": true, "conn": true, "connection_string": true,
	"url": true, "password": true, "user": true,
}

// IgnoreRule suppresses findings matching (Finding, Object). Object is an
// optional glob (path.Match syntax); omitted means "every object of this
// finding". Reason renders wherever the suppression shows. Expires is optional
// YYYY-MM-DD, after which the rule stops applying (B2-3).
type IgnoreRule struct {
	Finding string `toml:"finding"`
	Object  string `toml:"object"`
	Reason  string `toml:"reason"`
	Expires string `toml:"expires"`
}

// String is the stable identifier of a rule, recorded on a suppressed finding so
// an agent (or `config explain`) can name exactly which rule matched.
func (r IgnoreRule) String() string {
	if r.Object == "" {
		return r.Finding + " object=*"
	}
	return r.Finding + " object=" + r.Object
}

// raw mirrors the on-disk TOML before validation.
type raw struct {
	Schema     int               `toml:"schema"`
	Thresholds map[string]any    `toml:"thresholds"`
	Severity   map[string]string `toml:"severity"`
	Ignore     []IgnoreRule      `toml:"ignore"`
}

// Config is the validated, ready-to-apply configuration.
type Config struct {
	Schema             int
	Tunables           findings.Tunables  // thresholds, with overrides applied over defaults
	ThresholdOverrides map[string]float64 // config-key → value, only those the file set
	Severity           map[string]string  // finding id → remapped severity
	Ignore             []IgnoreRule
	Source             string          // path loaded from; "" means built-in defaults
	Warnings           []string        // non-fatal problems → Context.ConfigWarnings
	matched            map[string]bool // rules that fired this run (set by Apply, for B2-3 rot)
}

// MatchedRuleStrings returns the ignore rules that actually fired during the last
// Apply — whole-finding or row-level. The dead-rule detector (B2-3) uses this.
func (c *Config) MatchedRuleStrings() map[string]bool {
	if c.matched == nil {
		return map[string]bool{}
	}
	return c.matched
}

// Default is the zero-config: every default threshold, no overrides.
func Default() *Config {
	return &Config{
		Schema:             CurrentSchema,
		Tunables:           findings.DefaultTunables(),
		ThresholdOverrides: map[string]float64{},
		Severity:           map[string]string{},
	}
}

// Load discovers and parses the config. A discovery/read/parse error or a
// credential-shaped key is fatal (returned err); everything else that's wrong
// (unknown id, unknown threshold, bad glob) is a warning on the returned Config.
func Load(explicit string) (*Config, error) {
	pth, err := discover(explicit)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	cfg.Source = pth
	if pth == "" {
		return cfg, nil // no file: pure defaults
	}

	data, err := os.ReadFile(pth)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", pth, err)
	}
	var r raw
	md, err := toml.Decode(string(data), &r)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", pth, err)
	}

	// SECURITY (B2-1): refuse credentials before touching anything else.
	if bad := firstForbidden(md); bad != "" {
		return nil, fmt.Errorf(
			"config %s contains a credential-shaped key %q — pgbot never reads a connection string or password from a config file, because this file is meant to be committed. Pass the connection string as the command's argument (e.g. `pgbot inspect \"host=… dbname=…\"`) or set $DATABASE_URL ($PGBOT_DATABASE_URL and $PGSERVICE are also honored)",
			pth, bad)
	}

	if r.Schema != 0 && r.Schema != CurrentSchema {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"schema = %d but this pgbot understands schema %d; newer keys may be ignored", r.Schema, CurrentSchema))
	}
	if r.Schema != 0 {
		cfg.Schema = r.Schema
	}

	for k, v := range r.Thresholds {
		if err := applyThreshold(&cfg.Tunables, k, v); err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[thresholds] %s: %v", k, err))
		} else if fv, ok := toFloat(v); ok {
			cfg.ThresholdOverrides[k] = fv
		}
	}

	for id, sv := range r.Severity {
		if !findings.KnownID(id) {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[severity] %q is not a known finding id (typo?) — ignored", id))
			continue
		}
		if !validSeverity(sv) {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[severity] %s = %q is not one of critical/warn/info — ignored", id, sv))
			continue
		}
		cfg.Severity[id] = sv
	}

	for i, ig := range r.Ignore {
		if ig.Finding == "" {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[[ignore]] #%d has no `finding` — ignored", i+1))
			continue
		}
		if !findings.KnownID(ig.Finding) {
			// Keep it (forward-compatible: a newer pgbot may know it) but warn — a
			// typo'd id that silently never matches is the exact failure this exists
			// to prevent.
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[[ignore]] finding %q is not a known finding id (typo?) — kept but it will never match", ig.Finding))
		}
		if ig.Object != "" {
			if _, err := path.Match(ig.Object, "x"); err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[[ignore]] finding %s: object %q is not a valid glob (%v) — ignored", ig.Finding, ig.Object, err))
				continue
			}
		}
		if ig.Expires != "" {
			if _, err := time.Parse(expiryLayout, ig.Expires); err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[[ignore]] finding %s: expires %q is not YYYY-MM-DD — treated as no expiry", ig.Finding, ig.Expires))
				ig.Expires = ""
			}
		}
		cfg.Ignore = append(cfg.Ignore, ig)
	}

	sort.Strings(cfg.Warnings) // deterministic output (map iteration is not)
	return cfg, nil
}

// discover resolves the config path by precedence (first hit wins). An explicit
// --config or PGBOT_CONFIG that can't be read is a hard error — never a silent
// fall-through to a different file.
func discover(explicit string) (string, error) {
	if explicit != "" {
		if err := readable(explicit); err != nil {
			return "", fmt.Errorf("--config %s: %w", explicit, err)
		}
		return explicit, nil
	}
	if p := os.Getenv("PGBOT_CONFIG"); p != "" {
		if err := readable(p); err != nil {
			return "", fmt.Errorf("PGBOT_CONFIG=%s: %w", p, err)
		}
		return p, nil
	}
	if p := searchUp(".pgbot.toml"); p != "" {
		return p, nil
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		if p := filepath.Join(base, "pgbot", "config.toml"); exists(p) {
			return p, nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".config", "pgbot", "config.toml"); exists(p) {
			return p, nil
		}
	}
	return "", nil
}

// searchUp walks from the cwd to the filesystem root looking for name, so a
// .pgbot.toml at a repo root is found from any subdirectory.
func searchUp(name string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(dir, name)
		if exists(p) {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached root
		}
		dir = parent
	}
}

func exists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func readable(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	return f.Close()
}

// firstForbidden returns the first credential-shaped key present, or "".
func firstForbidden(md toml.MetaData) string {
	for _, k := range md.Keys() {
		leaf := strings.ToLower(k[len(k)-1])
		if forbiddenKeys[leaf] {
			return k.String()
		}
	}
	return ""
}

// applyThreshold overrides one tunable. Only the wired keys are accepted;
// anything else is reported so a typo doesn't silently do nothing.
func applyThreshold(t *findings.Tunables, key string, v any) error {
	f, ok := toFloat(v)
	if !ok {
		return fmt.Errorf("expected a number, got %T", v)
	}
	switch key {
	case "unused_index_min_size_mb":
		if f < 0 {
			return fmt.Errorf("must be ≥ 0")
		}
		t.UnusedIndexMinBytes = int64(f * (1 << 20))
	case "dead_ratio_warn":
		if f <= 0 || f > 1 {
			return fmt.Errorf("must be in (0,1]")
		}
		t.DeadRatioWarn = f
	case "replica_lag_warn_seconds":
		if f < 0 {
			return fmt.Errorf("must be ≥ 0")
		}
		t.ReplicaLagWarnSec = f
	default:
		return fmt.Errorf("unknown threshold key (overridable keys: unused_index_min_size_mb, dead_ratio_warn, replica_lag_warn_seconds)")
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func validSeverity(s string) bool {
	return s == "critical" || s == "warn" || s == "info"
}
