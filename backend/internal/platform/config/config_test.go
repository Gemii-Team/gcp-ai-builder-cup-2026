package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func key(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

// validEnv satisfies every serving role and migrate, leaving the defaulted and
// optional variables unset.
func validEnv() map[string]string {
	return map[string]string{
		"KIWI_PUBLIC_URL":           "http://localhost:8080",
		"KIWI_DATABASE_URL":         "postgres://kiwi_app@db:5432/kiwi",
		"KIWI_DATABASE_SYSTEM_URL":  "postgres://kiwi_system@db:5432/kiwi",
		"KIWI_MIGRATE_DATABASE_URL": "postgres://kiwi_owner@db:5432/kiwi",
		"KIWI_VALKEY_URL":           "redis://valkey:6379",
		"KIWI_BUS_PROVIDER":         "memory",
		"KIWI_BLOB_URL":             "file:///data/blobs",
		"KIWI_BLOB_SIGNING_KEY":     key(1),
		"KIWI_LLM_PROVIDER":         "openai",
		"KIWI_LLM_BASE_URL":         "http://llm.test",
		"KIWI_LLM_API_KEY":          "llm-credential-for-tests",
		"KIWI_LLM_MODEL_FLASH":      "flash-model",
		"KIWI_LLM_MODEL_PRO":        "pro-model",
		"KIWI_EMBED_BASE_URL":       "http://ollama:11434",
		"KIWI_EMBED_MODEL":          "embed-model",
		"KIWI_ASR_MODEL":            "asr-model",
		"KIWI_OAUTH_CLIENT_ID":      "oauth-client-id",
		"KIWI_OAUTH_CLIENT_SECRET":  "oauth-client-credential",
		"KIWI_TOKEN_ENC_KEY":        key(2),
	}
}

// soloEnv is validEnv without the in-process bus, for processes that run one
// role: the memory bus is refused unless api, worker and agents share a process.
func soloEnv() map[string]string {
	env := validEnv()
	delete(env, "KIWI_BUS_PROVIDER")
	return env
}

func with(env map[string]string, kv ...string) map[string]string {
	for i := 0; i < len(kv); i += 2 {
		env[kv[i]] = kv[i+1]
	}
	return env
}

func TestLoad_MissingRequiredNamesTheVariable(t *testing.T) {
	for name, set := range map[string]func(map[string]string){
		"absent": func(e map[string]string) { delete(e, "KIWI_DATABASE_URL") },
		"empty":  func(e map[string]string) { e["KIWI_DATABASE_URL"] = "" },
		"blank":  func(e map[string]string) { e["KIWI_DATABASE_URL"] = "  \t" },
	} {
		t.Run(name, func(t *testing.T) {
			env := soloEnv()
			set(env)
			_, err := Load([]Role{RoleAPI}, getenv(env))
			if err == nil {
				t.Fatal("Load succeeded without KIWI_DATABASE_URL")
			}
			if !strings.Contains(err.Error(), "KIWI_DATABASE_URL") {
				t.Fatalf("error does not name the variable: %v", err)
			}
			if strings.Contains(err.Error(), "KIWI_VALKEY_URL") {
				t.Fatalf("error names a variable that is set: %v", err)
			}
		})
	}
}

func TestLoad_RequiredSetsPerRole(t *testing.T) {
	// Env and OIDCIssuer are in the api set but always have a default, so they
	// can never be reported missing.
	want := map[Role][]string{
		RoleAPI: {
			"KIWI_PUBLIC_URL", "KIWI_DATABASE_URL", "KIWI_DATABASE_SYSTEM_URL", "KIWI_VALKEY_URL",
			"KIWI_BLOB_URL", "KIWI_BLOB_SIGNING_KEY", "KIWI_OAUTH_CLIENT_ID", "KIWI_OAUTH_CLIENT_SECRET",
			"KIWI_TOKEN_ENC_KEY",
		},
		RoleRealtime: {"KIWI_DATABASE_URL", "KIWI_VALKEY_URL"},
		RoleWorker: {
			"KIWI_PUBLIC_URL", "KIWI_DATABASE_URL", "KIWI_DATABASE_SYSTEM_URL", "KIWI_VALKEY_URL",
			"KIWI_BUS_PROVIDER", "KIWI_BLOB_URL", "KIWI_BLOB_SIGNING_KEY",
			"KIWI_LLM_PROVIDER", "KIWI_LLM_BASE_URL", "KIWI_LLM_API_KEY", "KIWI_LLM_MODEL_FLASH", "KIWI_LLM_MODEL_PRO",
			"KIWI_EMBED_BASE_URL", "KIWI_EMBED_MODEL", "KIWI_ASR_MODEL",
			"KIWI_OAUTH_CLIENT_ID", "KIWI_OAUTH_CLIENT_SECRET", "KIWI_TOKEN_ENC_KEY",
		},
		RoleAgents: {
			"KIWI_DATABASE_URL", "KIWI_VALKEY_URL", "KIWI_BUS_PROVIDER",
			"KIWI_LLM_PROVIDER", "KIWI_LLM_BASE_URL", "KIWI_LLM_API_KEY", "KIWI_LLM_MODEL_FLASH", "KIWI_LLM_MODEL_PRO",
			"KIWI_EMBED_BASE_URL", "KIWI_EMBED_MODEL",
		},
		RoleMigrate: {"KIWI_MIGRATE_DATABASE_URL"},
	}
	const prefix = "missing required environment variables: "
	for role, names := range want {
		t.Run(string(role), func(t *testing.T) {
			_, err := Load([]Role{role}, getenv(nil))
			if err == nil {
				t.Fatal("Load succeeded with an empty environment")
			}
			_, list, found := strings.Cut(err.Error(), prefix)
			if !found {
				t.Fatalf("error does not list missing variables: %v", err)
			}
			got := strings.Split(list, ", ")
			slices.Sort(got)
			wantSorted := slices.Sorted(slices.Values(names))
			if !slices.Equal(got, wantSorted) {
				t.Fatalf("missing set for %s\n got: %v\nwant: %v", role, got, wantSorted)
			}
		})
	}
}

func TestLoad_RequiredIsTheUnionOfTheRolesSets(t *testing.T) {
	_, err := Load([]Role{RoleRealtime, RoleMigrate}, getenv(nil))
	if err == nil {
		t.Fatal("Load succeeded with an empty environment")
	}
	for _, name := range []string{"KIWI_DATABASE_URL", "KIWI_VALKEY_URL", "KIWI_MIGRATE_DATABASE_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "KIWI_BLOB_URL") {
		t.Errorf("error names a variable no selected role requires: %v", err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(nil, getenv(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env != "local" {
		t.Errorf("Env = %q, want local", cfg.Env)
	}
	if cfg.OIDCIssuer != "https://accounts.google.com" {
		t.Errorf("OIDCIssuer = %q, want the Google issuer", cfg.OIDCIssuer)
	}
	if cfg.OIDCInternalURL != "" {
		t.Errorf("OIDCInternalURL = %q, want empty", cfg.OIDCInternalURL)
	}
	wantRoles := []Role{RoleAPI, RoleRealtime, RoleWorker, RoleAgents}
	if !slices.Equal(cfg.Roles, wantRoles) {
		t.Errorf("Roles = %v, want %v", cfg.Roles, wantRoles)
	}
	if cfg.WorkspaceName != "" || cfg.WorkspaceDomain != "" {
		t.Errorf("workspace presentation defaults = %q, %q, want empty", cfg.WorkspaceName, cfg.WorkspaceDomain)
	}
}

func TestLoad_ReadsEveryVariableIntoItsField(t *testing.T) {
	env := with(validEnv(),
		"KIWI_ENV", "local",
		"KIWI_ROLES", "api,worker,agents",
		"KIWI_OIDC_ISSUER", "http://mockoidc.test/issuer",
		"KIWI_OIDC_INTERNAL_URL", "http://mockoidc.internal",
		"KIWI_WORKSPACE_NAME", "Acme",
		"KIWI_WORKSPACE_DOMAIN", "acme.example",
	)
	cfg, err := Load(nil, getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		Env:               "local",
		Roles:             []Role{RoleAPI, RoleWorker, RoleAgents},
		PublicURL:         "http://localhost:8080",
		DatabaseURL:       "postgres://kiwi_app@db:5432/kiwi",
		DatabaseSystemURL: "postgres://kiwi_system@db:5432/kiwi",
		MigrateURL:        "postgres://kiwi_owner@db:5432/kiwi",
		ValkeyURL:         "redis://valkey:6379",
		BusProvider:       "memory",
		BlobURL:           "file:///data/blobs",
		BlobSigningKey:    bytes.Repeat([]byte{1}, 32),
		LLMProvider:       "openai",
		LLMBaseURL:        "http://llm.test",
		LLMAPIKey:         "llm-credential-for-tests",
		LLMModelFlash:     "flash-model",
		LLMModelPro:       "pro-model",
		EmbedBaseURL:      "http://ollama:11434",
		EmbedModel:        "embed-model",
		ASRModel:          "asr-model",
		OIDCIssuer:        "http://mockoidc.test/issuer",
		OIDCInternalURL:   "http://mockoidc.internal",
		OAuthClientID:     "oauth-client-id",
		OAuthClientSecret: "oauth-client-credential",
		TokenEncKey:       bytes.Repeat([]byte{2}, 32),
		WorkspaceName:     "Acme",
		WorkspaceDomain:   "acme.example",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load mapped variables to the wrong fields\n got: %+v\nwant: %+v", cfg, want)
	}
}

func TestLoad_TrimsWhitespaceAroundValues(t *testing.T) {
	cfg, err := Load([]Role{RoleRealtime}, getenv(with(soloEnv(), "KIWI_VALKEY_URL", "  redis://valkey:6379\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ValkeyURL != "redis://valkey:6379" {
		t.Errorf("ValkeyURL = %q", cfg.ValkeyURL)
	}
}

func TestLoad_ProdRejectsOverriddenOIDC(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string // empty: Load must succeed
	}{
		{"prod with a non-Google issuer", with(soloEnv(), "KIWI_ENV", "prod", "KIWI_OIDC_ISSUER", "http://mockoidc.test"), "KIWI_OIDC_ISSUER"},
		{"prod with an internal URL", with(soloEnv(), "KIWI_ENV", "prod", "KIWI_OIDC_INTERNAL_URL", "http://mockoidc.internal"), "KIWI_OIDC_INTERNAL_URL"},
		{"prod with the Google issuer", with(soloEnv(), "KIWI_ENV", "prod", "KIWI_OIDC_ISSUER", "https://accounts.google.com"), ""},
		{"prod with defaults", with(soloEnv(), "KIWI_ENV", "prod"), ""},
		{"local with a non-Google issuer", with(soloEnv(), "KIWI_OIDC_ISSUER", "http://mockoidc.test", "KIWI_OIDC_INTERNAL_URL", "http://mockoidc.internal"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]Role{RoleAPI}, getenv(tc.env))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Load: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatal("Load succeeded")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error does not name %s: %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoad_RejectsAnUnknownEnv(t *testing.T) {
	_, err := Load([]Role{RoleAPI}, getenv(with(soloEnv(), "KIWI_ENV", "staging")))
	if err == nil || !strings.Contains(err.Error(), "KIWI_ENV") {
		t.Fatalf("want an error naming KIWI_ENV, got %v", err)
	}
}

func TestLoad_UnknownRoleNamesKIWI_ROLES(t *testing.T) {
	if _, err := ParseRoles("api,bogus"); err == nil || !strings.Contains(err.Error(), "KIWI_ROLES") || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("ParseRoles: want an error naming KIWI_ROLES and bogus, got %v", err)
	}
	if _, err := Load(nil, getenv(with(validEnv(), "KIWI_ROLES", "api,bogus"))); err == nil || !strings.Contains(err.Error(), "KIWI_ROLES") {
		t.Errorf("Load: want an error naming KIWI_ROLES, got %v", err)
	}
	if _, err := Load([]Role{"bogus"}, getenv(validEnv())); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("Load with an explicit role: want an error naming bogus, got %v", err)
	}
}

func TestParseRoles(t *testing.T) {
	all := []Role{RoleAPI, RoleRealtime, RoleWorker, RoleAgents}
	for _, tc := range []struct {
		in      string
		want    []Role
		wantErr bool
	}{
		{"", all, false},
		{"  ", all, false},
		{"api,realtime,worker,agents", all, false},
		{"worker, api", []Role{RoleWorker, RoleAPI}, false},
		{"realtime", []Role{RoleRealtime}, false},
		{"api,api", nil, true},
		{"api,,worker", nil, true},
		{"API", nil, true},
		// migrate is a subcommand, not something `serve` can run.
		{"migrate", nil, true},
	} {
		got, err := ParseRoles(tc.in)
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "KIWI_ROLES") {
				t.Errorf("ParseRoles(%q): want an error naming KIWI_ROLES, got %v", tc.in, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("ParseRoles(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestLoad_MemoryBusRequiresAPIWorkerAgentsInOneProcess(t *testing.T) {
	for _, tc := range []struct {
		roles   string
		wantErr bool
	}{
		{"api,realtime", true},
		{"api,worker", true},
		{"worker,agents", true},
		{"realtime", true},
		{"api,worker,agents", false},
		{"api,realtime,worker,agents", false},
	} {
		t.Run(tc.roles, func(t *testing.T) {
			env := with(validEnv(), "KIWI_ROLES", tc.roles, "KIWI_BUS_PROVIDER", "memory")
			_, err := Load(nil, getenv(env))
			switch {
			case !tc.wantErr && err != nil:
				t.Fatalf("Load: %v", err)
			case tc.wantErr && err == nil:
				t.Fatal("Load succeeded")
			case tc.wantErr && !strings.Contains(err.Error(), "KIWI_BUS_PROVIDER=memory"):
				t.Fatalf("error does not name KIWI_BUS_PROVIDER=memory: %v", err)
			}
		})
	}

	// The migrate Job shares the ConfigMap but runs no bus.
	if _, err := Load([]Role{RoleMigrate}, getenv(with(validEnv(), "KIWI_BUS_PROVIDER", "memory"))); err != nil {
		t.Errorf("migrate alone with the memory bus: %v", err)
	}
}

func TestLoad_RejectsProvidersNotBuiltLocally(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"KIWI_BUS_PROVIDER", "pubsub"},
		{"KIWI_BUS_PROVIDER", "nats"},
		{"KIWI_BLOB_URL", "gs://x"},
		{"KIWI_BLOB_URL", "s3://bucket?endpoint=http://rustfs:9000"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			_, err := Load(nil, getenv(with(validEnv(), tc.name, tc.value)))
			if err == nil {
				t.Fatal("Load succeeded")
			}
			if !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), "not built in the local MVP") {
				t.Fatalf("error must name %s and say it is not built in the local MVP: %v", tc.name, err)
			}
		})
	}
}

func TestLoad_BlobURLMustBeAFileURLWithAnAbsolutePath(t *testing.T) {
	for _, bad := range []string{"file://host/data", "file:relative/path", "file:///", "/data/blobs", "::"} {
		_, err := Load([]Role{RoleAPI}, getenv(with(soloEnv(), "KIWI_BLOB_URL", bad)))
		if err == nil || !strings.Contains(err.Error(), "KIWI_BLOB_URL") {
			t.Errorf("KIWI_BLOB_URL=%q: want an error naming KIWI_BLOB_URL, got %v", bad, err)
		}
	}
}

func TestLoad_KeysMustBeBase64Of32Bytes(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 16))
	for _, name := range []string{"KIWI_BLOB_SIGNING_KEY", "KIWI_TOKEN_ENC_KEY"} {
		for label, bad := range map[string]string{"not base64": "%%%not-base64%%%", "16 bytes": short} {
			t.Run(name+" "+label, func(t *testing.T) {
				_, err := Load([]Role{RoleAPI}, getenv(with(soloEnv(), name, bad)))
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("want an error naming %s, got %v", name, err)
				}
				if strings.Contains(err.Error(), bad) {
					t.Fatalf("error leaks the value: %v", err)
				}
			})
		}
	}
}

func TestLoad_ReportsEveryProblemAtOnce(t *testing.T) {
	env := with(validEnv(), "KIWI_ENV", "staging", "KIWI_BUS_PROVIDER", "pubsub")
	delete(env, "KIWI_VALKEY_URL")
	_, err := Load(nil, getenv(env))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, name := range []string{"KIWI_ENV", "KIWI_BUS_PROVIDER", "KIWI_VALKEY_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
}

func readEnvExample(t *testing.T) []string {
	t.Helper()
	// The test runs in backend/internal/platform/config; the file sits at the repository root.
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	return strings.Split(string(raw), "\n")
}

func TestEnvExampleListsEveryVariable(t *testing.T) {
	lines := readEnvExample(t)
	known := map[string]bool{}
	for _, name := range append(slices.Clone(AllEnvVars), LocalScriptEnvVars...) {
		known[name] = true
		if !slices.Contains(lines, name+"=") {
			t.Errorf(".env.example has no empty %s= line", name)
		}
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "KIWI_") {
			continue
		}
		name, value, found := strings.Cut(trimmed, "=")
		if !found {
			t.Errorf(".env.example line %d is not an assignment: %q", i+1, trimmed)
			continue
		}
		if value != "" {
			t.Errorf(".env.example line %d gives %s a value; the file lists names only", i+1, name)
		}
		if !known[name] {
			t.Errorf(".env.example line %d lists %s, which config does not read", i+1, name)
		}
	}
}

func TestAllEnvVarsAreUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range append(slices.Clone(AllEnvVars), LocalScriptEnvVars...) {
		if !strings.HasPrefix(name, "KIWI_") {
			t.Errorf("%s lacks the KIWI_ prefix", name)
		}
		if seen[name] {
			t.Errorf("%s is listed twice", name)
		}
		seen[name] = true
	}
}

// TestLoad_ReturnsTheZeroConfigOnError: a Config that failed validation is not half
// filled, so nothing that ignores the error can start on it.
func TestLoad_ReturnsTheZeroConfigOnError(t *testing.T) {
	env := with(validEnv(), "KIWI_ENV", "staging")
	cfg, err := Load(nil, getenv(env))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Fatalf("Load returned a partly filled Config with its error: %v", cfg)
	}
}

// secretFields is the policy: what a log line or a panic message must never carry.
// The config tags each of them secret:"true"; the test fails when one loses the tag.
var secretFields = []string{
	"DatabaseURL", "DatabaseSystemURL", "MigrateURL", "ValkeyURL", "BlobURL", "BlobSigningKey",
	"LLMBaseURL", "LLMAPIKey", "EmbedBaseURL", "OAuthClientSecret", "TokenEncKey",
}

func TestConfig_RedactsSecretsEverywhereItIsFormatted(t *testing.T) {
	cfg := Config{
		Env:               "local",
		Roles:             []Role{RoleAPI, RoleWorker},
		PublicURL:         "http://public.example:8080",
		DatabaseURL:       "postgres://kiwi_app:app-secret-pw@db-host-1:5432/kiwi",
		DatabaseSystemURL: "postgres://kiwi_system:system-secret-pw@db-host-2:5432/kiwi",
		MigrateURL:        "postgres://kiwi_owner:owner-secret-pw@db-host-3:5432/kiwi",
		ValkeyURL:         "redis://:valkey-secret-pw@valkey-host:6379",
		BusProvider:       "memory",
		BlobURL:           "file:///data/secret-blob-dir",
		BlobSigningKey:    []byte("0123456789abcdef0123456789abcdef"),
		LLMProvider:       "openai",
		LLMBaseURL:        "https://llm-secret-host.example",
		LLMAPIKey:         "sk-secret-key",
		LLMModelFlash:     "flash-model",
		LLMModelPro:       "pro-model",
		EmbedBaseURL:      "http://embed-secret-host:11434",
		EmbedModel:        "embed-model",
		ASRModel:          "asr-model",
		OIDCIssuer:        "http://issuer.example/mockoidc",
		OIDCInternalURL:   "http://mockoidc:8090/mockoidc",
		OAuthClientID:     "client-id-visible",
		OAuthClientSecret: "client-secret-value",
		TokenEncKey:       []byte("abcdefghijklmnopqrstuvwxyz012345"),
		WorkspaceName:     "Acme",
		WorkspaceDomain:   "acme.example",
	}
	secrets := []string{
		"app-secret-pw", "system-secret-pw", "owner-secret-pw", "valkey-secret-pw", "db-host-1", "valkey-host",
		"secret-blob-dir", "0123456789abcdef", base64.StdEncoding.EncodeToString(cfg.BlobSigningKey),
		"llm-secret-host", "sk-secret-key", "embed-secret-host", "client-secret-value",
		"abcdefghijklmnopqrstuvwxyz012345", base64.StdEncoding.EncodeToString(cfg.TokenEncKey),
	}
	visible := []string{
		"local", "api", "worker", "http://public.example:8080", "memory", "openai", "flash-model", "pro-model",
		"embed-model", "asr-model", "http://issuer.example/mockoidc", "http://mockoidc:8090/mockoidc",
		"client-id-visible", "Acme", "acme.example", redacted,
	}

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("starting", "config", cfg)
	var logText bytes.Buffer
	slog.New(slog.NewTextHandler(&logText, nil)).Info("starting", slog.Any("config", cfg))
	renderings := map[string]string{
		"String":      cfg.String(),
		"%v":          fmt.Sprintf("%v", cfg),
		"%+v":         fmt.Sprintf("%+v", cfg),
		"%#v":         fmt.Sprintf("%#v", cfg),
		"%v of a ptr": fmt.Sprintf("%v", &cfg),
		"slog JSON":   logged.String(),
		"slog text":   logText.String(),
		"Sprint":      fmt.Sprint(cfg),
		"%s":          fmt.Sprintf("<%s>", cfg),
		"%q":          fmt.Sprintf("%q", cfg),
		"%x":          fmt.Sprintf("%x", cfg),
		"embedded":    fmt.Sprintf("%v", struct{ C Config }{cfg}),
	}
	for name, out := range renderings {
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s shows the secret %q:\n%s", name, s, out)
			}
		}
	}
	// The readable renderings still say what the process runs with.
	for _, name := range []string{"String", "slog JSON", "slog text"} {
		for _, v := range visible {
			if !strings.Contains(renderings[name], v) {
				t.Errorf("%s does not show %q:\n%s", name, v, renderings[name])
			}
		}
	}
}

func TestConfig_EverySecretFieldIsTaggedAndNoOtherIs(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	for _, name := range secretFields {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("Config has no field %s", name)
		}
		if f.Tag.Get("secret") != "true" {
			t.Errorf("%s is not tagged secret:\"true\": it would be logged in the clear", name)
		}
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Tag.Get("secret") == "true" && !slices.Contains(secretFields, f.Name) {
			t.Errorf("%s is tagged secret but is not in the policy of this test: add it or drop the tag", f.Name)
		}
	}
}
