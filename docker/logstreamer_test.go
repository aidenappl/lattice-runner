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
		{"6-char-deploy-suffix", "openbucket-zixn9i", "openbucket"},
		{"6-char-alpha-only", "myapp-abcdef", "myapp"},
		{"6-char-alphanumeric", "myapp-a1b2c3", "myapp"},
		{"uppercase-not-stripped", "myapp-ZIXN9I", "myapp-ZIXN9I"},
		{"3-char-not-stripped", "myapp-abc", "myapp-abc"},
		{"7-char-not-stripped", "myapp-abcdefg", "myapp-abcdefg"},
		{"5-char-not-stripped", "myapp-abcde", "myapp-abcde"},
		{"multi-hyphen-with-suffix", "my-long-app-zixn9i", "my-long-app"},
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
