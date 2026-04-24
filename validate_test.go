package main

import (
	"strings"
	"testing"
)

func TestValidContainerName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"simple", "myapp", true},
		{"with-hyphen", "my-app", true},
		{"with-underscore", "my_app", true},
		{"with-dot", "my.app", true},
		{"with-slash", "a/b", true},
		{"single-char", "a", true},
		{"max-length", strings.Repeat("a", 128), true},
		{"digits", "123", true},
		{"mixed", "My-App_v2.0/prod", true},
		{"empty", "", false},
		{"over-max", strings.Repeat("a", 129), false},
		{"has-space", "has space", false},
		{"has-semicolon", "has;semi", false},
		{"has-dollar", "has$dollar", false},
		{"has-ampersand", "has&amp", false},
		{"has-backtick", "has`tick", false},
		{"has-pipe", "has|pipe", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validContainerName(tt.input)
			if got != tt.want {
				t.Errorf("validContainerName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
