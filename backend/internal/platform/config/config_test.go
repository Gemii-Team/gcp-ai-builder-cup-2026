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
	"time"
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

// The defaults of the new settings are what the full stack ran with before they existed.
func TestLoad_DefaultsKeepTheFullStackAsItWas(t *testing.T) {
	cfg, err := Load(nil, getenv(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CacheProvider != "valkey" {
		t.Errorf("CacheProvider = %q, want valkey", cfg.CacheProvider)
	}
	if cfg.DBAppMaxConns != 20 || cfg.DBSystemMaxConns != 16 {
		t.Errorf("pools = %d app + %d system, want 20 + 16", cfg.DBAppMaxConns, cfg.DBSystemMaxConns)
	}
	if cfg.DBMaxConnIdle != 30*time.Minute {
		t.Errorf("DBMaxConnIdle = %v, want 30m (pgx's default)", cfg.DBMaxConnIdle)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.WebDir != "" {
		t.Errorf("WebDir = %q, want empty: no static files", cfg.WebDir)
	}
}

func TestLoad_PoolsAndIdleTimeCanBeOverridden(t *testing.T) {
	env := with(validEnv(), "KIWI_DB_APP_MAX_CONNS", "6", "KIWI_DB_SYSTEM_MAX_CONNS", "6", "KIWI_DB_MAX_CONN_IDLE", "90s")
	cfg, err := Load(nil, getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBAppMaxConns != 6 || cfg.DBSystemMaxConns != 6 || cfg.DBMaxConnIdle != 90*time.Second {
		t.Errorf("pools = %d app + %d system, idle %v; want 6 + 6, 90s", cfg.DBAppMaxConns, cfg.DBSystemMaxConns, cfg.DBMaxConnIdle)
	}
}

func TestLoad_RejectsBadPoolsAndIdleTimes(t *testing.T) {
	for _, tc := range []struct{ name, variable, value, want string }{
		{"app pool of zero", "KIWI_DB_APP_MAX_CONNS", "0", "KIWI_DB_APP_MAX_CONNS must be at least 1"},
		{"negative app pool", "KIWI_DB_APP_MAX_CONNS", "-3", "KIWI_DB_APP_MAX_CONNS must be at least 1"},
		{"app pool that is not a number", "KIWI_DB_APP_MAX_CONNS", "many", "KIWI_DB_APP_MAX_CONNS must be a whole number"},
		{"system pool of zero", "KIWI_DB_SYSTEM_MAX_CONNS", "0", "KIWI_DB_SYSTEM_MAX_CONNS must be at least 1"},
		// The relay holds 4 connections, and the callback, sessions and poll need 2 more.
		{"system pool below the relay's need", "KIWI_DB_SYSTEM_MAX_CONNS", "5", "KIWI_DB_SYSTEM_MAX_CONNS must be at least 6"},
		{"system pool of the relay's size", "KIWI_DB_SYSTEM_MAX_CONNS", "4", "KIWI_DB_SYSTEM_MAX_CONNS must be at least 6"},
		{"idle time that is not a duration", "KIWI_DB_MAX_CONN_IDLE", "soon", "KIWI_DB_MAX_CONN_IDLE must be a duration"},
		{"idle time of zero", "KIWI_DB_MAX_CONN_IDLE", "0s", "KIWI_DB_MAX_CONN_IDLE must be longer than zero"},
		{"negative idle time", "KIWI_DB_MAX_CONN_IDLE", "-1m", "KIWI_DB_MAX_CONN_IDLE must be longer than zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(nil, getenv(with(validEnv(), tc.variable, tc.value)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.want)
			}
			// A bad value is reported once, not once as unparsable and again as too small.
			if n := strings.Count(err.Error(), tc.variable); n != 1 {
				t.Errorf("%s is named %d times in %q, want once", tc.variable, n, err)
			}
		})
	}
	// The smallest system pool the relay cannot starve is accepted.
	if _, err := Load(nil, getenv(with(validEnv(), "KIWI_DB_SYSTEM_MAX_CONNS", "6"))); err != nil {
		t.Errorf("system pool of 6: %v", err)
	}
}

func TestLoad_ListenAddr(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"nothing set", nil, ":8080"},
		{"PORT alone (Cloud Run)", []string{"PORT", "9090"}, ":9090"},
		{"PORT with blanks", []string{"PORT", " 9090 "}, ":9090"},
		{"KIWI_LISTEN_ADDR alone", []string{"KIWI_LISTEN_ADDR", "127.0.0.1:7000"}, "127.0.0.1:7000"},
		{"KIWI_LISTEN_ADDR wins over PORT", []string{"KIWI_LISTEN_ADDR", ":7000", "PORT", "9090"}, ":7000"},
		{"an empty KIWI_LISTEN_ADDR falls through to PORT", []string{"KIWI_LISTEN_ADDR", "", "PORT", "9090"}, ":9090"},
		{"an empty PORT is no PORT", []string{"PORT", ""}, ":8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(nil, getenv(with(validEnv(), tc.env...)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ListenAddr != tc.want {
				t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"PORT that is not a number", []string{"PORT", "http"}},
		{"PORT out of range", []string{"PORT", "70000"}},
		{"PORT of zero", []string{"PORT", "0"}},
		{"address without a port", []string{"KIWI_LISTEN_ADDR", "127.0.0.1"}},
		{"address with a bad port", []string{"KIWI_LISTEN_ADDR", ":eighty"}},
		{"address with port 0", []string{"KIWI_LISTEN_ADDR", ":0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(nil, getenv(with(validEnv(), tc.env...)))
			if err == nil || !strings.Contains(err.Error(), "KIWI_LISTEN_ADDR") {
				t.Fatalf("Load error = %v, want it to name KIWI_LISTEN_ADDR", err)
			}
		})
	}
	// PORT is ignored, and so not validated, once KIWI_LISTEN_ADDR is set.
	if _, err := Load(nil, getenv(with(validEnv(), "KIWI_LISTEN_ADDR", ":7000", "PORT", "not-a-port"))); err != nil {
		t.Errorf("a bad PORT behind a set KIWI_LISTEN_ADDR: %v", err)
	}
}

func TestLoad_WebDirMustBeAbsolute(t *testing.T) {
	cfg, err := Load(nil, getenv(with(validEnv(), "KIWI_WEB_DIR", "/app/web")))
	if err != nil || cfg.WebDir != "/app/web" {
		t.Fatalf("Load = %q, %v; want /app/web", cfg.WebDir, err)
	}
	for _, dir := range []string{"web", "./web", "../web", "app/web"} {
		if _, err := Load(nil, getenv(with(validEnv(), "KIWI_WEB_DIR", dir))); err == nil || !strings.Contains(err.Error(), "KIWI_WEB_DIR") {
			t.Errorf("KIWI_WEB_DIR=%q: error = %v, want it to name KIWI_WEB_DIR", dir, err)
		}
	}
}

func TestLoad_CacheProvider(t *testing.T) {
	cfg, err := Load(nil, getenv(with(validEnv(), "KIWI_CACHE_PROVIDER", "memory")))
	if err != nil || cfg.CacheProvider != "memory" {
		t.Fatalf("Load = %q, %v; want memory", cfg.CacheProvider, err)
	}
	_, err = Load(nil, getenv(with(validEnv(), "KIWI_CACHE_PROVIDER", "redis")))
	if err == nil || !strings.Contains(err.Error(), "KIWI_CACHE_PROVIDER") {
		t.Errorf("an unknown provider: error = %v, want it to name KIWI_CACHE_PROVIDER", err)
	}
}

// The memory cache is the process's own, so a process that does not run every serving
// role would issue tickets nobody redeems and publish frames nobody forwards.
func TestLoad_MemoryCacheRequiresEveryServingRoleInOneProcess(t *testing.T) {
	for _, tc := range []struct {
		roles   string
		wantErr bool
	}{
		{"api", true},
		{"realtime", true},
		{"api,worker,agents", true}, // enough for the memory bus, not for the memory cache
		{"api,realtime,worker", true},
		{"realtime,worker,agents", true},
		{"api,realtime,worker,agents", false},
		{"agents,worker,realtime,api", false},
		{"", false}, // the default is every serving role
	} {
		t.Run(tc.roles, func(t *testing.T) {
			env := with(validEnv(), "KIWI_ROLES", tc.roles, "KIWI_CACHE_PROVIDER", "memory")
			_, err := Load(nil, getenv(env))
			switch {
			case !tc.wantErr && err != nil:
				t.Fatalf("Load: %v", err)
			case tc.wantErr && err == nil:
				t.Fatal("Load accepted the memory cache in a process that does not run every role")
			case tc.wantErr && !strings.Contains(err.Error(), "KIWI_CACHE_PROVIDER=memory"):
				t.Fatalf("error does not name KIWI_CACHE_PROVIDER=memory: %v", err)
			}
		})
	}

	// The valkey cache has no such rule: processes may split the roles.
	if _, err := Load(nil, getenv(with(validEnv(), "KIWI_ROLES", "realtime", "KIWI_BUS_PROVIDER", "", "KIWI_CACHE_PROVIDER", "valkey"))); err != nil {
		t.Errorf("a realtime-only process on Valkey: %v", err)
	}
	// The migrate Job shares the ConfigMap but uses no cache.
	if _, err := Load([]Role{RoleMigrate}, getenv(with(validEnv(), "KIWI_CACHE_PROVIDER", "memory"))); err != nil {
		t.Errorf("migrate alone with the memory cache: %v", err)
	}
}

func TestLoad_ValkeyURLIsRequiredOnlyForTheValkeyCache(t *testing.T) {
	env := validEnv()
	delete(env, "KIWI_VALKEY_URL")
	_, err := Load(nil, getenv(env))
	if err == nil || !strings.Contains(err.Error(), "KIWI_VALKEY_URL") {
		t.Fatalf("Valkey cache without KIWI_VALKEY_URL: error = %v, want it to name the variable", err)
	}
	// An unknown provider keeps the address required, so fixing the provider alone is not enough.
	_, err = Load(nil, getenv(with(env, "KIWI_CACHE_PROVIDER", "redis")))
	if err == nil || !strings.Contains(err.Error(), "KIWI_VALKEY_URL") {
		t.Errorf("unknown provider without KIWI_VALKEY_URL: error = %v, want it to name the variable too", err)
	}
	cfg, err := Load(nil, getenv(with(env, "KIWI_CACHE_PROVIDER", "memory")))
	if err != nil {
		t.Fatalf("memory cache without KIWI_VALKEY_URL: %v", err)
	}
	if cfg.ValkeyURL != "" {
		t.Errorf("ValkeyURL = %q, want empty", cfg.ValkeyURL)
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
		"KIWI_WEB_DIR", "/app/web",
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
		CacheProvider:     "valkey",
		DBAppMaxConns:     20,
		DBSystemMaxConns:  16,
		DBMaxConnIdle:     30 * time.Minute,
		ListenAddr:        ":8080",
		WebDir:            "/app/web",
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

// publicFields is the other half of the policy: what a log line may show. Every field of
// Config is in exactly one of the two lists, so a field added later cannot be shown in the
// clear by default: the test below fails until someone has decided which list it is in.
var publicFields = []string{
	"Env", "Roles", "PublicURL", "BusProvider", "LLMProvider", "LLMModelFlash", "LLMModelPro", "EmbedModel", "ASRModel",
	"OIDCIssuer", "OIDCInternalURL", "OAuthClientID", "WorkspaceName", "WorkspaceDomain",
	"CacheProvider", "DBAppMaxConns", "DBSystemMaxConns", "DBMaxConnIdle", "ListenAddr", "WebDir",
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
		LLMAPIKey:         "llm-credential-value",
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
		"llm-secret-host", "llm-credential-value", "embed-secret-host", "client-secret-value",
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

// The classification is fail-closed: a field of Config that is in neither list, or in both,
// fails here, so a credential added without a decision is a red build and not a line in a log.
func TestConfig_EveryFieldIsClassifiedSecretOrPublicInExactlyOneList(t *testing.T) {
	typ := reflect.TypeFor[Config]()
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		fields = append(fields, name)
		secret, public := slices.Contains(secretFields, name), slices.Contains(publicFields, name)
		switch {
		case !secret && !public:
			t.Errorf("Config.%s is in neither secretFields nor publicFields: decide whether a log line may show it, then list it (and tag it secret:\"true\" if not)", name)
		case secret && public:
			t.Errorf("Config.%s is in both secretFields and publicFields: it is one or the other", name)
		}
	}
	for _, list := range [][]string{secretFields, publicFields} {
		for _, name := range list {
			if !slices.Contains(fields, name) {
				t.Errorf("%s is listed but Config has no such field: drop it", name)
			}
		}
	}
}
