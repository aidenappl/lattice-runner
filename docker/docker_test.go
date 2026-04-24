package docker

import "testing"

func TestValidateEnvKey(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"uppercase", "MY_VAR", false},
		{"lowercase", "my_var", false},
		{"underscore-prefix", "_PRIVATE", false},
		{"single-char", "a", false},
		{"mixed-with-digits", "VAR_123", false},
		{"empty", "", true},
		{"starts-with-digit", "1VAR", true},
		{"has-hyphen", "my-var", true},
		{"has-space", "MY VAR", true},
		{"has-dot", "my.var", true},
		{"has-equals", "VAR=val", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEnvKey(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateEnvKey(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateHostPath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"named-volume", "myvolume", false},
		{"safe-path", "/data/app", false},
		{"opt-path", "/opt/myapp", false},
		{"var-lib", "/var/lib/myapp", false},
		{"etc-blocked", "/etc", true},
		{"etc-subpath", "/etc/hosts", true},
		{"root-blocked", "/root", true},
		{"root-subpath", "/root/.ssh", true},
		{"home-blocked", "/home", true},
		{"home-subpath", "/home/user", true},
		{"docker-sock", "/var/run/docker.sock", true},
		{"run-docker-sock", "/run/docker.sock", true},
		{"proc-blocked", "/proc", true},
		{"sys-blocked", "/sys", true},
		{"dev-blocked", "/dev", true},
		{"boot-blocked", "/boot", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHostPath(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostPath(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
