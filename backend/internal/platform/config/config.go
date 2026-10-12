// Package config is the one place that reads the process environment. Every
// variable the binary understands is a tagged field of Config; AllEnvVars is
// derived from those tags, so .env.example cannot drift from the code without
// a failing test.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
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
)

// servingRoles are the roles `kiwi serve` can run; their order is the default of KIWI_ROLES.
var servingRoles = []Role{RoleAPI, RoleRealtime, RoleWorker, RoleAgents}

// defaults apply when a variable is unset or empty.
var defaults = map[string]string{
	"KIWI_ENV":         envLocal,
	"KIWI_OIDC_ISSUER": googleIssuer,
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
	ValkeyURL  string `env:"KIWI_VALKEY_URL" roles:"api,realtime,worker,agents" secret:"true"`

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

	fields := reflect.ValueOf(&cfg).Elem()
	for i := range fields.NumField() {
		f := fields.Type().Field(i)
		name := f.Tag.Get("env")
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			value = defaults[name]
		}
		if value == "" {
			if requiredBy(f.Tag.Get("roles"), roles) {
				missing = append(missing, name)
			}
			continue
		}
		switch dst := fields.Field(i).Addr().Interface().(type) {
		case *[]Role:
			// Resolved above: KIWI_ROLES decides which sets are required.
		case *string:
			*dst = value
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

	if c.BlobURL != "" {
		if err := checkBlobURL(c.BlobURL); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
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
