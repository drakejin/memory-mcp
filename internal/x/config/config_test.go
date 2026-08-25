package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// clearEnv unsets every variable Load reads so a table case starts from the
// documented defaults rather than the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envHome, envUsername, envS3Bucket, envS3Region, envAWSProfile,
		envOpenSearchURL, envNeo4jURL, envEpisodicTTLDays, envListenAddr,
	} {
		t.Setenv(key, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "/tmp/fake-home")
	t.Setenv(envUsername, "jin")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	want := Config{
		Home:            filepath.Join("/tmp/fake-home", homeParentDir, homeDirName),
		Username:        "jin",
		S3Bucket:        DefaultS3Bucket,
		S3Region:        DefaultS3Region,
		AWSProfile:      DefaultAWSProfile,
		OpenSearchURL:   DefaultOpenSearchURL,
		Neo4jURL:        DefaultNeo4jURL,
		Neo4jUser:       DefaultNeo4jUser,
		Neo4jPassword:   DefaultNeo4jPassword,
		EpisodicTTLDays: DefaultEpisodicTTLDays,
		ListenAddr:      DefaultListenAddr,
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv(envHome, "/srv/memory")
	t.Setenv(envUsername, "someone")
	t.Setenv(envS3Bucket, "other-bucket")
	t.Setenv(envS3Region, "us-east-1")
	t.Setenv(envAWSProfile, "other-profile")
	t.Setenv(envOpenSearchURL, "http://127.0.0.1:19200")
	t.Setenv(envNeo4jURL, "bolt://127.0.0.1:17687")
	t.Setenv(envEpisodicTTLDays, "7")
	t.Setenv(envListenAddr, "localhost:9999")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	want := Config{
		Home:            "/srv/memory",
		Username:        "someone",
		S3Bucket:        "other-bucket",
		S3Region:        "us-east-1",
		AWSProfile:      "other-profile",
		OpenSearchURL:   "http://127.0.0.1:19200",
		Neo4jURL:        "bolt://127.0.0.1:17687",
		Neo4jUser:       DefaultNeo4jUser,
		Neo4jPassword:   DefaultNeo4jPassword,
		EpisodicTTLDays: 7,
		ListenAddr:      "localhost:9999",
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadUsesOSUserWhenUsernameUnset(t *testing.T) {
	clearEnv(t)
	t.Setenv(envHome, "/srv/memory")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Username == "" {
		t.Error("Username must fall back to the OS user, got empty")
	}
}

func TestLoadFailures(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr error
	}{
		{
			name:    "non-numeric ttl is invalid",
			env:     map[string]string{envEpisodicTTLDays: "soon"},
			wantErr: errs.ErrInvalid,
		},
		{
			name:    "zero ttl is invalid",
			env:     map[string]string{envEpisodicTTLDays: "0"},
			wantErr: errs.ErrInvalid,
		},
		{
			name:    "negative ttl is invalid",
			env:     map[string]string{envEpisodicTTLDays: "-3"},
			wantErr: errs.ErrInvalid,
		},
		{
			name:    "routable bind address is rejected",
			env:     map[string]string{envListenAddr: "0.0.0.0:8420"},
			wantErr: errs.ErrInvalid,
		},
		{
			// With DJ_MEMORY_HOME unset the OS home is consulted, and an empty
			// $HOME makes that lookup fail.
			name:    "unresolvable home is internal",
			env:     map[string]string{envHome: "", "HOME": ""},
			wantErr: errs.ErrInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(envHome, "/srv/memory")
			t.Setenv(envUsername, "jin")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatal("Load() = nil, want an error")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Load() error = %v, want kind %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadErrorNeverLeaksTheValue(t *testing.T) {
	clearEnv(t)
	t.Setenv(envHome, "/srv/memory")
	t.Setenv(envUsername, "jin")
	t.Setenv(envEpisodicTTLDays, "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want an error")
	}
	var domain *errs.Error
	if !errors.As(err, &domain) {
		t.Fatalf("Load() error = %T, want *errs.Error", err)
	}
	if domain.Msg == "" {
		t.Error("the client-facing message must name the offending variable")
	}
	if got := domain.Fields["value"]; got != "not-a-number" {
		t.Errorf("Fields[value] = %v, want the rejected raw value for logs", got)
	}
}

func TestValidate(t *testing.T) {
	valid := Config{
		Home:            "/srv/memory",
		Username:        "jin",
		S3Bucket:        DefaultS3Bucket,
		S3Region:        DefaultS3Region,
		AWSProfile:      DefaultAWSProfile,
		OpenSearchURL:   DefaultOpenSearchURL,
		Neo4jURL:        DefaultNeo4jURL,
		Neo4jUser:       DefaultNeo4jUser,
		Neo4jPassword:   DefaultNeo4jPassword,
		EpisodicTTLDays: DefaultEpisodicTTLDays,
		ListenAddr:      DefaultListenAddr,
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "spec defaults are valid", mutate: func(*Config) {}},
		{name: "localhost bind is valid", mutate: func(c *Config) { c.ListenAddr = "localhost:8420" }},
		{name: "empty home", mutate: func(c *Config) { c.Home = "" }, wantErr: true},
		{name: "empty username", mutate: func(c *Config) { c.Username = "" }, wantErr: true},
		{name: "empty bucket", mutate: func(c *Config) { c.S3Bucket = "" }, wantErr: true},
		{name: "wildcard bind", mutate: func(c *Config) { c.ListenAddr = "0.0.0.0:8420" }, wantErr: true},
		{name: "lan bind", mutate: func(c *Config) { c.ListenAddr = "192.168.0.10:8420" }, wantErr: true},
		{name: "empty bind", mutate: func(c *Config) { c.ListenAddr = "" }, wantErr: true},
		{name: "zero ttl", mutate: func(c *Config) { c.EpisodicTTLDays = 0 }, wantErr: true},
		{name: "negative ttl", mutate: func(c *Config) { c.EpisodicTTLDays = -1 }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)

			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("Validate() error kind = %v, want invalid", err)
			}
		})
	}
}

func TestSpecLimitsAreStable(t *testing.T) {
	// These constants are the deterministic thresholds of §3.1 and §6; other
	// packages read them instead of re-declaring their own.
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"episodic ttl days", DefaultEpisodicTTLDays, 30},
		{"project file bytes", MaxProjectFileBytes, 5 << 20},
		{"project records", MaxProjectRecords, 5000},
		{"document chunk bytes", DocumentChunkBytes, 2048},
		{"max document chunks", MaxDocumentChunks, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}
}
