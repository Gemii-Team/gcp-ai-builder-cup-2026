// Package config is the one place that reads the process environment. Every
// variable the binary understands is a tagged field of Config; AllEnvVars is
// derived from those tags, so .env.example cannot drift from the code without
// a failing test.
package config

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Role is one of the processes the binary can act as. A single process runs
// the union of the roles listed in KIWI_ROLES.
type Role string

// The roles. RoleMigrate is run by `kiwi migrate`, never listed in KIWI_ROLES.
const (
	RoleAPI      Role = "api"
	RoleRealtime Role = "realtime"
	RoleWorker   Role = "worker"
	RoleAgents   Role = "agents"
	RoleMigrate  Role = "migrate"
)

const (
	rolesVar     = "KIWI_ROLES"
	envLocal     = "local"
	envProd      = "prod"
	googleIssuer = "https://accounts.google.com"
	keyBytes     = 32

	// portVar is the variable Cloud Run (and most platforms) set to the port the
	// container must listen on. It is not a KIWI_ variable, so Load reads it directly
	// and it is not a tagged field.
	portVar = "PORT"
	// DefaultListenAddr is where kiwi listens when neither KIWI_LISTEN_ADDR nor PORT is set.
	DefaultListenAddr = ":8080"
)

// The cache providers (KIWI_CACHE_PROVIDER).
const (
	// CacheValkey keeps sessions, realtime tickets, OAuth state, the members cache and
	// the realtime fan-out in Valkey, which every process of a deployment shares.
	CacheValkey = "valkey"
	// CacheMemory keeps the ephemeral state in the process; sessions go to Postgres.
	// It is only valid when one process runs every serving role.
	CacheMemory = "memory"
)

// The database pool sizes and the idle time, as they were before they became
// configurable: the defaults keep the full stack as it was.
const (
	DefaultDBAppMaxConns    = 20
	DefaultDBSystemMaxConns = 16
	// DefaultDBMaxConnIdle is pgx's own default (pgxpool's defaultMaxConnIdleTime), which
	// is what the pools used while the value was not configurable.
	DefaultDBMaxConnIdle = 30 * time.Minute

	// RelayConcurrency is how many outbox rows the worker holds in flight. The relay
	// holds one system connection per row for as long as its handlers work.
	RelayConcurrency = 4
	// MinDBSystemMaxConns is the smallest system pool the relay cannot starve: its
	// RelayConcurrency connections, and two more for the sign-in callback, the session
	// lookups of the Postgres session store and the Meet poll.
	MinDBSystemMaxConns = RelayConcurrency + 2

	// minDBMaxConnIdle is the shortest idle time accepted; pgx only looks for idle
	// connections once a minute, so less is no more precise, just a way to churn.
	minDBMaxConnIdle = time.Second
)

// servingRoles are the roles `kiwi serve` can run; their order is the default of KIWI_ROLES.
var servingRoles = []Role{RoleAPI, RoleRealtime, RoleWorker, RoleAgents}

// defaults apply when a variable is unset or empty. KIWI_LISTEN_ADDR's default depends
// on PORT and is worked out by defaultFor.
var defaults = map[string]string{
	"KIWI_ENV":                 envLocal,
	"KIWI_OIDC_ISSUER":         googleIssuer,
	"KIWI_CACHE_PROVIDER":      CacheValkey,
	"KIWI_DB_APP_MAX_CONNS":    strconv.Itoa(DefaultDBAppMaxConns),
	"KIWI_DB_SYSTEM_MAX_CONNS": strconv.Itoa(DefaultDBSystemMaxConns),
	"KIWI_DB_MAX_CONN_IDLE":    DefaultDBMaxConnIdle.String(),
}

// Config is the validated process configuration. The env tag names the
// variable; the roles tag lists the roles that refuse to start without it. A
// field tagged secret:"true" can carry a credential or an address that is one
// (a password in a URL, a key, the model endpoint): String, GoString and LogValue
// replace it, so logging or printing a Config never shows it.
// Values that no listed role requires are optional.
type Config struct {
	// Env is "local" or "prod".
	Env string `env:"KIWI_ENV" roles:"api"`
	// Roles is the set of roles this process runs.
	Roles []Role `env:"KIWI_ROLES"`

	// PublicURL is the browser-visible origin; signed blob URLs are built on it.
	PublicURL string `env:"KIWI_PUBLIC_URL" roles:"api,worker"`

	// DatabaseURL connects as kiwi_app, which Postgres row-level security binds.
	DatabaseURL string `env:"KIWI_DATABASE_URL" roles:"api,realtime,worker,agents" secret:"true"`
	// DatabaseSystemURL connects as kiwi_system, which bypasses row-level security.
	DatabaseSystemURL string `env:"KIWI_DATABASE_SYSTEM_URL" roles:"api,worker" secret:"true"`
	// MigrateURL connects as kiwi_owner, which owns the tables.
	MigrateURL string `env:"KIWI_MIGRATE_DATABASE_URL" roles:"migrate" secret:"true"`
	// ValkeyURL is required only when CacheProvider is valkey.
	ValkeyURL string `env:"KIWI_VALKEY_URL" roles:"api,realtime,worker,agents" secret:"true"`
	// CacheProvider is "valkey" (the default) or "memory". memory keeps the realtime
	// fan-out, tickets and OAuth state in the process and the sessions in Postgres, so
	// it needs one process that runs every serving role.
	CacheProvider string `env:"KIWI_CACHE_PROVIDER"`

	// DBAppMaxConns and DBSystemMaxConns size the two connection pools of a process.
	// The system pool must hold the outbox relay's connections and more: validate
	// enforces MinDBSystemMaxConns.
	DBAppMaxConns    int `env:"KIWI_DB_APP_MAX_CONNS" min:"1" max:"1000"`
	DBSystemMaxConns int `env:"KIWI_DB_SYSTEM_MAX_CONNS" min:"1" max:"1000"`
	// DBMaxConnIdle is how long an idle pooled connection is kept before it is closed.
	DBMaxConnIdle time.Duration `env:"KIWI_DB_MAX_CONN_IDLE"`

	// ListenAddr is the address the process listens on: KIWI_LISTEN_ADDR, else
	// ":"+PORT when PORT is set (Cloud Run), else ":8080".
	ListenAddr string `env:"KIWI_LISTEN_ADDR"`
	// WebDir is the absolute directory of the built web app. Empty serves none.
	WebDir string `env:"KIWI_WEB_DIR"`

	// BusProvider is "memory", the only bus built locally.
	BusProvider string `env:"KIWI_BUS_PROVIDER" roles:"worker,agents"`
	// BlobURL is file:///<absolute path>, the only blob store built locally.
	BlobURL string `env:"KIWI_BLOB_URL" roles:"api,worker" secret:"true"`
	// BlobSigningKey is the decoded 32-byte HMAC key of signed blob URLs.
	BlobSigningKey []byte `env:"KIWI_BLOB_SIGNING_KEY" roles:"api,worker" secret:"true"`

	LLMProvider   string `env:"KIWI_LLM_PROVIDER" roles:"worker,agents"`
	LLMBaseURL    string `env:"KIWI_LLM_BASE_URL" roles:"worker,agents" secret:"true"`
	LLMAPIKey     string `env:"KIWI_LLM_API_KEY" roles:"worker,agents" secret:"true"`
	LLMModelFlash string `env:"KIWI_LLM_MODEL_FLASH" roles:"worker,agents"`
	LLMModelPro   string `env:"KIWI_LLM_MODEL_PRO" roles:"worker,agents"`
	EmbedBaseURL  string `env:"KIWI_EMBED_BASE_URL" roles:"worker,agents" secret:"true"`
	EmbedModel    string `env:"KIWI_EMBED_MODEL" roles:"worker,agents"`
	ASRModel      string `env:"KIWI_ASR_MODEL" roles:"worker"`

	// OIDCIssuer is Google's issuer; outside prod a mock provider may replace it.
	OIDCIssuer string `env:"KIWI_OIDC_ISSUER" roles:"api"`
	// OIDCInternalURL, when set, is where discovery and keys are fetched while
	// the issuer claim is still checked against OIDCIssuer. Never set in prod.
	OIDCInternalURL   string `env:"KIWI_OIDC_INTERNAL_URL"`
	OAuthClientID     string `env:"KIWI_OAUTH_CLIENT_ID" roles:"api,worker"`
	OAuthClientSecret string `env:"KIWI_OAUTH_CLIENT_SECRET" roles:"api,worker" secret:"true"`
	// TokenEncKey is the decoded 32-byte AES-GCM key that encrypts refresh tokens.
	TokenEncKey []byte `env:"KIWI_TOKEN_ENC_KEY" roles:"api,worker" secret:"true"`

	// WorkspaceName and WorkspaceDomain are presentational only (the sign-in
	// heading and domain hint); authorisation never reads them.
	WorkspaceName   string `env:"KIWI_WORKSPACE_NAME"`
	WorkspaceDomain string `env:"KIWI_WORKSPACE_DOMAIN"`
}

// redacted stands in for the value of a secret field that is set.
const redacted = "[REDACTED]"

// field is one Config field as it is shown: its Go name and its value, which is
// redacted for a secret.
type field struct{ name, value string }

// redactedFields lists every field of c in declaration order. A secret that is set
// shows as "[REDACTED]" and one that is not as "", so a log still tells whether a
// variable arrived.
func redactedFields(c Config) []field {
	v := reflect.ValueOf(c)
	out := make([]field, 0, v.NumField())
	for i := range v.NumField() {
		f := v.Type().Field(i)
		var shown string
		switch val := v.Field(i).Interface().(type) {
		case string:
			shown = val
		case int:
			shown = strconv.Itoa(val)
		case time.Duration:
			shown = val.String()
		case []byte:
			if len(val) > 0 {
				shown = "<set>"
			}
		case []Role:
			shown = joinRoles(val)
		default:
			panic(fmt.Sprintf("config: field %s has unsupported type %s", f.Name, f.Type))
		}
		if f.Tag.Get("secret") == "true" && shown != "" {
			shown = redacted
		}
		out = append(out, field{f.Name, shown})
	}
	return out
}

// String implements fmt.Stringer: %v, %+v, %s and Println show the redacted form.
func (c Config) String() string {
	var b strings.Builder
	b.WriteString("config.Config{")
	for i, f := range redactedFields(c) {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s:%s", f.name, f.value)
	}
	b.WriteString("}")
	return b.String()
}

// GoString implements fmt.GoStringer, which %#v uses instead of String.
func (c Config) GoString() string { return c.String() }

// LogValue implements slog.LogValuer: a Config logged as an attribute is a group of
// its redacted fields.
func (c Config) LogValue() slog.Value {
	fields := redactedFields(c)
	attrs := make([]slog.Attr, len(fields))
	for i, f := range fields {
		attrs[i] = slog.String(f.name, f.value)
	}
	return slog.GroupValue(attrs...)
}

// AllEnvVars lists every variable Load reads, in Config field order.
var AllEnvVars = envNames()

// LocalScriptEnvVars are read by scripts/local/*.sh, not by Load.
var LocalScriptEnvVars = []string{"KIWI_DB_PASSWORD"}

func envNames() []string {
	t := reflect.TypeFor[Config]()
	names := make([]string, t.NumField())
	for i := range names {
		names[i] = t.Field(i).Tag.Get("env")
	}
	return names
}

// ParseRoles parses the value of KIWI_ROLES: a comma-separated list of serving
// roles. An empty value selects all of them.
func ParseRoles(s string) ([]Role, error) {
	if strings.TrimSpace(s) == "" {
		return slices.Clone(servingRoles), nil
	}
	var roles []Role
	for part := range strings.SplitSeq(s, ",") {
		r := Role(strings.TrimSpace(part))
		switch {
		case !slices.Contains(servingRoles, r):
			return nil, fmt.Errorf("%s: unknown role %q (valid: %s)", rolesVar, string(r), joinRoles(servingRoles))
		case slices.Contains(roles, r):
			return nil, fmt.Errorf("%s: role %q is listed twice", rolesVar, string(r))
		}
		roles = append(roles, r)
	}
	return roles, nil
}

func joinRoles(roles []Role) string {
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = string(r)
	}
	return strings.Join(names, ", ")
}

// Load reads the environment through getenv and returns the configuration for
// a process running roles. With no roles it runs the ones KIWI_ROLES selects
// (all serving roles when that is unset). The required variables are the union
// of the roles' sets; every problem found is reported in one error that names
// each variable and never a secret value. When it reports a problem the Config is
// the zero value, never a partly filled one.
func Load(roles []Role, getenv func(string) string) (Config, error) {
	if len(roles) == 0 {
		parsed, err := ParseRoles(getenv(rolesVar))
		if err != nil {
			return Config{}, err
		}
		roles = parsed
	}
	for _, r := range roles {
		if !slices.Contains(servingRoles, r) && r != RoleMigrate {
			return Config{}, fmt.Errorf("unknown role %q", string(r))
		}
	}

	cfg := Config{Roles: slices.Clone(roles)}
	var problems []error
	var missing []string

	// With the memory cache nothing reads Valkey, so its address is not required.
	// An unknown provider is reported by validate; it keeps the address required.
	valkeyUnused := cmp.Or(strings.TrimSpace(getenv("KIWI_CACHE_PROVIDER")), defaults["KIWI_CACHE_PROVIDER"]) == CacheMemory

	fields := reflect.ValueOf(&cfg).Elem()
	for i := range fields.NumField() {
		f := fields.Type().Field(i)
		name := f.Tag.Get("env")
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			value = defaultFor(name, getenv)
		}
		if value == "" {
			if requiredBy(f.Tag.Get("roles"), roles) && (name != "KIWI_VALKEY_URL" || !valkeyUnused) {
				missing = append(missing, name)
			}
			continue
		}
		switch dst := fields.Field(i).Addr().Interface().(type) {
		case *[]Role:
			// Resolved above: KIWI_ROLES decides which sets are required.
		case *string:
			*dst = value
		case *int:
			n, err := strconv.Atoi(value)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s must be a whole number", name))
				continue
			}
			if minimum, err := strconv.Atoi(f.Tag.Get("min")); err == nil && n < minimum {
				problems = append(problems, fmt.Errorf("%s must be at least %d", name, minimum))
				continue
			}
			// A pool this large only exhausts Postgres's max_connections: a typo, not a setting.
			if maximum, err := strconv.Atoi(f.Tag.Get("max")); err == nil && n > maximum {
				problems = append(problems, fmt.Errorf("%s must be at most %d", name, maximum))
				continue
			}
			*dst = n
		case *time.Duration:
			d, err := time.ParseDuration(value)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s must be a duration such as 5m or 90s", name))
				continue
			}
			if d < minDBMaxConnIdle {
				problems = append(problems, fmt.Errorf("%s must be at least %s: a shorter time makes the pool reconnect constantly", name, minDBMaxConnIdle))
				continue
			}
			*dst = d
		case *[]byte:
			raw, err := base64.StdEncoding.DecodeString(value)
			if err != nil || len(raw) != keyBytes {
				problems = append(problems, fmt.Errorf("%s must be base64 of %d bytes (for example `openssl rand -base64 %d`)", name, keyBytes, keyBytes))
				continue
			}
			*dst = raw
		default:
			panic(fmt.Sprintf("config: field %s has unsupported type %s", f.Name, f.Type))
		}
	}

	if len(missing) > 0 {
		problems = append([]error{fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))}, problems...)
	}
	if err := errors.Join(append(problems, cfg.validate()...)...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// defaultFor is the value a variable takes when it is unset or empty.
func defaultFor(name string, getenv func(string) string) string {
	if name == "KIWI_LISTEN_ADDR" {
		if port := strings.TrimSpace(getenv(portVar)); port != "" {
			return ":" + port
		}
		return DefaultListenAddr
	}
	return defaults[name]
}

// requiredBy reports whether any of roles appears in the comma-separated tag.
func requiredBy(tag string, roles []Role) bool {
	for name := range strings.SplitSeq(tag, ",") {
		if slices.Contains(roles, Role(name)) {
			return true
		}
	}
	return false
}

// validate checks the values that are set; absent ones are reported as missing.
func (c Config) validate() []error {
	var errs []error

	if c.Env != envLocal && c.Env != envProd {
		errs = append(errs, fmt.Errorf("KIWI_ENV must be %q or %q, got %q", envLocal, envProd, c.Env))
	}
	if c.Env == envProd && (c.OIDCIssuer != googleIssuer || c.OIDCInternalURL != "") {
		errs = append(errs, fmt.Errorf("KIWI_ENV=prod requires KIWI_OIDC_ISSUER to be %s and KIWI_OIDC_INTERNAL_URL to be unset", googleIssuer))
	}

	if c.BusProvider != "" && c.BusProvider != "memory" {
		errs = append(errs, fmt.Errorf("KIWI_BUS_PROVIDER=%s is not built in the local MVP (only memory)", c.BusProvider))
	}
	// The migrate Job shares the ConfigMap but runs no bus.
	inOneProcess := c.hasRoles(RoleAPI, RoleWorker, RoleAgents)
	if c.BusProvider == "memory" && !inOneProcess && !c.runsAlone(RoleMigrate) {
		errs = append(errs, fmt.Errorf("KIWI_BUS_PROVIDER=memory is in-process: KIWI_ROLES must include api, worker and agents in one process (got %s)", joinRoles(c.Roles)))
	}

	if c.CacheProvider != CacheValkey && c.CacheProvider != CacheMemory {
		errs = append(errs, fmt.Errorf("KIWI_CACHE_PROVIDER must be %q or %q, got %q", CacheValkey, CacheMemory, c.CacheProvider))
	}
	// The memory cache is the process's own: tickets are issued by the api role and
	// redeemed by the realtime role, frames are published by the worker's relay and the
	// agents and fanned out by the realtime role. Every serving role must share the
	// process, or a ticket or a frame would never reach the role that needs it. (The
	// bus rule above asks for less, because the gateway never touches the bus.) The
	// migrate Job shares the ConfigMap but uses no cache.
	if c.CacheProvider == CacheMemory && !c.hasRoles(servingRoles...) && !c.runsAlone(RoleMigrate) {
		errs = append(errs, fmt.Errorf("KIWI_CACHE_PROVIDER=memory is in-process: KIWI_ROLES must include %s in one process (got %s)", joinRoles(servingRoles), joinRoles(c.Roles)))
	}

	// The relay holds one system connection per row in flight; a smaller pool lets it
	// starve sign-in, the session lookups and the Meet poll. A value below 1 is
	// reported when it is read.
	if c.DBSystemMaxConns >= 1 && c.DBSystemMaxConns < MinDBSystemMaxConns {
		errs = append(errs, fmt.Errorf("KIWI_DB_SYSTEM_MAX_CONNS must be at least %d: the outbox relay holds up to %d connections while its jobs run, and sign-in, session lookups and the Meet poll need two more", MinDBSystemMaxConns, RelayConcurrency))
	}

	if c.ListenAddr != "" {
		if err := checkListenAddr(c.ListenAddr); err != nil {
			errs = append(errs, err)
		}
	}
	if c.WebDir != "" && !filepath.IsAbs(c.WebDir) {
		errs = append(errs, errors.New("KIWI_WEB_DIR must be an absolute directory"))
	}

	if c.BlobURL != "" {
		if err := checkBlobURL(c.BlobURL); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// checkListenAddr accepts host:port, where the host may be empty and the port is
// 1 to 65535. A listen address that came from PORT is named after PORT, since that is
// the variable somebody has to fix.
func checkListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err == nil {
		var n uint64
		if n, err = strconv.ParseUint(port, 10, 16); err == nil && n == 0 {
			err = errors.New("port 0")
		}
	}
	if err != nil {
		return errors.New("KIWI_LISTEN_ADDR (or PORT, when it is empty) must be host:port with a port from 1 to 65535, for example :8080")
	}
	return nil
}

func (c Config) runsAlone(r Role) bool {
	return len(c.Roles) == 1 && c.Roles[0] == r
}

func (c Config) hasRoles(want ...Role) bool {
	for _, r := range want {
		if !slices.Contains(c.Roles, r) {
			return false
		}
	}
	return true
}

// checkBlobURL never echoes the URL: other schemes may carry credentials.
func checkBlobURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("KIWI_BLOB_URL is not a valid URL")
	}
	if u.Scheme != "file" {
		return fmt.Errorf("KIWI_BLOB_URL scheme %q is not built in the local MVP (only file:///<absolute path>)", u.Scheme)
	}
	if u.Host != "" || !strings.HasPrefix(u.Path, "/") || u.Path == "/" {
		return errors.New("KIWI_BLOB_URL must be file:///<absolute path>")
	}
	return nil
}
