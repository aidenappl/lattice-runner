package backup

import (
	"strings"
	"testing"
)

func TestNewDestination(t *testing.T) {
	tests := []struct {
		name     string
		destType string
		config   map[string]any
		wantErr  bool
	}{
		{
			"s3-valid",
			"s3",
			map[string]any{
				"bucket":            "my-bucket",
				"region":            "us-east-1",
				"access_key_id":     "AKID",
				"secret_access_key": "SECRET",
			},
			false,
		},
		{
			"samba-valid",
			"samba",
			map[string]any{
				"server":   "192.168.1.100",
				"share":    "backups",
				"username": "admin",
				"password": "pass123",
			},
			false,
		},
		{
			"unknown-type",
			"ftp",
			map[string]any{},
			true,
		},
		{
			"empty-type",
			"",
			map[string]any{},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest, err := NewDestination(tt.destType, tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewDestination(%q) error = %v, wantErr %v", tt.destType, err, tt.wantErr)
			}
			if !tt.wantErr && dest == nil {
				t.Errorf("NewDestination(%q) returned nil destination without error", tt.destType)
			}
		})
	}
}

func TestNewDestinationGDrive(t *testing.T) {
	// google_drive requires valid OAuth config and creates a real service,
	// so we can only test the validation path (missing fields).
	t.Run("google_drive-missing-fields", func(t *testing.T) {
		_, err := NewDestination("google_drive", map[string]any{})
		if err == nil {
			t.Error("expected error for google_drive with missing fields")
		}
	})
}

func TestGetString(t *testing.T) {
	config := map[string]any{
		"name":   "test",
		"count":  42,
		"nested": map[string]any{"key": "val"},
	}

	tests := []struct {
		name string
		key  string
		want string
	}{
		{"existing-key", "name", "test"},
		{"missing-key", "nonexistent", ""},
		{"wrong-type-int", "count", ""},
		{"wrong-type-map", "nested", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getString(config, tt.key)
			if got != tt.want {
				t.Errorf("getString(config, %q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestNewS3Destination(t *testing.T) {
	validConfig := map[string]any{
		"bucket":            "my-bucket",
		"region":            "us-east-1",
		"access_key_id":     "AKID",
		"secret_access_key": "SECRET",
	}

	t.Run("valid", func(t *testing.T) {
		dest, err := newS3Destination(validConfig)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dest == nil {
			t.Fatal("expected non-nil destination")
		}
		if dest.bucket != "my-bucket" {
			t.Errorf("bucket = %q, want %q", dest.bucket, "my-bucket")
		}
	})

	t.Run("with-endpoint-and-prefix", func(t *testing.T) {
		config := map[string]any{
			"bucket":            "my-bucket",
			"region":            "us-east-1",
			"access_key_id":     "AKID",
			"secret_access_key": "SECRET",
			"endpoint":          "http://minio:9000",
			"path_prefix":       "backups/db",
		}
		dest, err := newS3Destination(config)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dest.pathPrefix != "backups/db" {
			t.Errorf("pathPrefix = %q, want %q", dest.pathPrefix, "backups/db")
		}
	})

	missingFieldTests := []struct {
		name   string
		remove string
	}{
		{"missing-bucket", "bucket"},
		{"missing-region", "region"},
		{"missing-access-key", "access_key_id"},
		{"missing-secret-key", "secret_access_key"},
	}

	for _, tt := range missingFieldTests {
		t.Run(tt.name, func(t *testing.T) {
			config := make(map[string]any)
			for k, v := range validConfig {
				config[k] = v
			}
			delete(config, tt.remove)
			_, err := newS3Destination(config)
			if err == nil {
				t.Errorf("expected error when %s is missing", tt.remove)
			}
		})
	}

	t.Run("empty-config", func(t *testing.T) {
		_, err := newS3Destination(map[string]any{})
		if err == nil {
			t.Error("expected error for empty config")
		}
	})
}

func TestNewSambaDestination(t *testing.T) {
	validConfig := map[string]any{
		"server":   "192.168.1.100",
		"share":    "backups",
		"username": "admin",
		"password": "pass123",
	}

	t.Run("valid", func(t *testing.T) {
		dest, err := newSambaDestination(validConfig)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dest.server != "192.168.1.100" {
			t.Errorf("server = %q, want %q", dest.server, "192.168.1.100")
		}
		if dest.share != "backups" {
			t.Errorf("share = %q, want %q", dest.share, "backups")
		}
	})

	t.Run("valid-with-path", func(t *testing.T) {
		config := map[string]any{
			"server":   "192.168.1.100",
			"share":    "backups",
			"username": "admin",
			"password": "pass123",
			"path":     "databases/prod",
		}
		dest, err := newSambaDestination(config)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dest.path != "databases/prod" {
			t.Errorf("path = %q, want %q", dest.path, "databases/prod")
		}
	})

	missingFieldTests := []struct {
		name   string
		remove string
	}{
		{"missing-server", "server"},
		{"missing-share", "share"},
		{"missing-username", "username"},
		{"missing-password", "password"},
	}

	for _, tt := range missingFieldTests {
		t.Run(tt.name, func(t *testing.T) {
			config := make(map[string]any)
			for k, v := range validConfig {
				config[k] = v
			}
			delete(config, tt.remove)
			_, err := newSambaDestination(config)
			if err == nil {
				t.Errorf("expected error when %s is missing", tt.remove)
			}
		})
	}

	t.Run("username-with-percent", func(t *testing.T) {
		config := map[string]any{
			"server":   "192.168.1.100",
			"share":    "backups",
			"username": "admin%domain",
			"password": "pass123",
		}
		_, err := newSambaDestination(config)
		if err == nil {
			t.Error("expected error for username containing %")
		}
		if err != nil && !strings.Contains(err.Error(), "%") {
			t.Errorf("error should mention %%, got: %v", err)
		}
	})
}

func TestNewGDriveDestination(t *testing.T) {
	t.Run("missing-client-id", func(t *testing.T) {
		config := map[string]any{
			"client_secret": "secret",
			"refresh_token": "token",
		}
		_, err := newGDriveDestination(config)
		if err == nil {
			t.Error("expected error when client_id is missing")
		}
	})

	t.Run("missing-client-secret", func(t *testing.T) {
		config := map[string]any{
			"client_id":     "id",
			"refresh_token": "token",
		}
		_, err := newGDriveDestination(config)
		if err == nil {
			t.Error("expected error when client_secret is missing")
		}
	})

	t.Run("missing-refresh-token", func(t *testing.T) {
		config := map[string]any{
			"client_id":     "id",
			"client_secret": "secret",
		}
		_, err := newGDriveDestination(config)
		if err == nil {
			t.Error("expected error when refresh_token is missing")
		}
	})

	t.Run("all-fields-missing", func(t *testing.T) {
		_, err := newGDriveDestination(map[string]any{})
		if err == nil {
			t.Error("expected error when all fields are missing")
		}
	})
}

func TestSmbEscape(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no-quotes", "hello world", "hello world"},
		{"single-double-quote", `say "hello"`, `say \"hello\"`},
		{"multiple-quotes", `"a" and "b"`, `\"a\" and \"b\"`},
		{"empty-string", "", ""},
		{"only-quote", `"`, `\"`},
		{"consecutive-quotes", `""`, `\"\"`},
		{"no-special-chars", "simple", "simple"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := smbEscape(tt.input)
			if got != tt.want {
				t.Errorf("smbEscape(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
