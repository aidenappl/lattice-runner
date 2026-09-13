package cmd

import (
	"os"
	"strings"
	"testing"
)

// TestServiceTemplateDockerDependency pins how the unit depends on Docker.
// Requires=docker.service must never come back: one failed Docker start job
// leaves the runner "Dependency failed" and never retried. See the comment on
// serviceTemplate.
func TestServiceTemplateDockerDependency(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		present bool
	}{
		{"ordered after docker", "After=network.target docker.service", true},
		{"pulls docker in at boot", "Wants=docker.service", true},
		{"restarts along with docker", "PartOf=docker.service", true},
		{"restarts when the process exits", "Restart=always", true},
		{"no hard requirement on docker", "Requires=docker.service", false},
	}

	lines := strings.Split(serviceTemplate, "\n")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := false
			for _, l := range lines {
				if strings.TrimSpace(l) == tt.line {
					found = true
					break
				}
			}
			if found != tt.present {
				t.Errorf("serviceTemplate contains %q = %v, want %v", tt.line, found, tt.present)
			}
		})
	}
}

// TestUpdateScriptUnitMatchesServiceTemplate keeps the two copies of the unit in
// lockstep. deploy/update.sh writes its own copy for workers installed by the
// one-liner; when the two drift, a fix lands in one install path and not the
// other — which is exactly how Requires=docker.service survived in both.
func TestUpdateScriptUnitMatchesServiceTemplate(t *testing.T) {
	raw, err := os.ReadFile("../deploy/update.sh")
	if err != nil {
		t.Fatalf("read deploy/update.sh: %v", err)
	}
	script := string(raw)

	const open = "<<'EOF'\n"
	start := strings.Index(script, open)
	if start < 0 {
		t.Fatal("deploy/update.sh: unit heredoc (<<'EOF') not found")
	}
	body := script[start+len(open):]
	end := strings.Index(body, "\nEOF\n")
	if end < 0 {
		t.Fatal("deploy/update.sh: unit heredoc terminator not found")
	}
	unit := body[:end+1]

	if unit != serviceTemplate {
		t.Errorf("deploy/update.sh unit differs from serviceTemplate\n--- update.sh ---\n%s\n--- serviceTemplate ---\n%s", unit, serviceTemplate)
	}
}
