package docker

import "testing"

func TestCanonicalContainerName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no-suffix", "myapp", "myapp"},
		{"retired-suffix", "myapp-retired-1234567890", "myapp"},
		{"lattice-updating", "myapp-lattice-updating", "myapp"},
		{"marker-deploy-suffix", "openbucket-ltczixn9i", "openbucket"},
		{"marker-alpha-only", "myapp-ltcabcdef", "myapp"},
		{"marker-alphanumeric", "myapp-ltca1b2c3", "myapp"},
		// Bare 6-char segments are NO LONGER stripped — they collide with real names.
		{"bare-6-char-not-stripped", "openbucket-zixn9i", "openbucket-zixn9i"},
		{"worker-not-stripped", "myapp-worker", "myapp-worker"},
		{"server-not-stripped", "myapp-server", "myapp-server"},
		{"canary-not-stripped", "myapp-canary", "myapp-canary"},
		{"uppercase-not-stripped", "myapp-ltcZIXN9I", "myapp-ltcZIXN9I"},
		{"marker-only-not-stripped", "myapp-ltc", "myapp-ltc"},
		{"marker-short-not-stripped", "myapp-ltcabc", "myapp-ltcabc"},
		{"multi-hyphen-with-suffix", "my-long-app-ltczixn9i", "my-long-app"},
		{"retired-in-middle", "my-retired-app", "my"}, // -retired- is detected and stripped
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanonicalContainerName(tt.input)
			if got != tt.want {
				t.Errorf("CanonicalContainerName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
