package deploy

import (
	"encoding/json"
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
		{"max-length", strings.Repeat("a", 128), true},
		{"empty", "", false},
		{"over-max", strings.Repeat("a", 129), false},
		{"has-space", "has space", false},
		{"has-semicolon", "has;semi", false},
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

func TestDeploymentSpecValidate(t *testing.T) {
	t.Run("valid-minimal", func(t *testing.T) {
		spec := DeploymentSpec{
			Containers: []ContainerSpec{
				{Name: "myapp", Image: "nginx"},
			},
		}
		if err := spec.Validate(); err != nil {
			t.Errorf("valid spec returned error: %v", err)
		}
	})

	t.Run("valid-multiple-containers", func(t *testing.T) {
		spec := DeploymentSpec{
			Containers: []ContainerSpec{
				{Name: "web", Image: "nginx", Tag: "latest"},
				{Name: "api", Image: "myapi", Tag: "v1.0"},
			},
			Networks: []NetworkSpec{{Name: "app-net", Driver: "bridge"}},
			Volumes:  []VolumeSpec{{Name: "data", Driver: "local"}},
		}
		if err := spec.Validate(); err != nil {
			t.Errorf("valid spec returned error: %v", err)
		}
	})

	t.Run("too-many-containers", func(t *testing.T) {
		containers := make([]ContainerSpec, 101)
		for i := range containers {
			containers[i] = ContainerSpec{Name: "app", Image: "nginx"}
		}
		spec := DeploymentSpec{Containers: containers}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for >100 containers")
		}
	})

	t.Run("too-many-networks", func(t *testing.T) {
		networks := make([]NetworkSpec, 51)
		for i := range networks {
			networks[i] = NetworkSpec{Name: "net"}
		}
		spec := DeploymentSpec{
			Containers: []ContainerSpec{{Name: "app", Image: "nginx"}},
			Networks:   networks,
		}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for >50 networks")
		}
	})

	t.Run("too-many-volumes", func(t *testing.T) {
		volumes := make([]VolumeSpec, 51)
		for i := range volumes {
			volumes[i] = VolumeSpec{Name: "vol"}
		}
		spec := DeploymentSpec{
			Containers: []ContainerSpec{{Name: "app", Image: "nginx"}},
			Volumes:    volumes,
		}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for >50 volumes")
		}
	})

	t.Run("invalid-container-name", func(t *testing.T) {
		spec := DeploymentSpec{
			Containers: []ContainerSpec{
				{Name: "invalid name!", Image: "nginx"},
			},
		}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for invalid container name")
		}
	})

	t.Run("missing-image", func(t *testing.T) {
		spec := DeploymentSpec{
			Containers: []ContainerSpec{
				{Name: "myapp", Image: ""},
			},
		}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for missing image")
		}
	})

	t.Run("shell-metacharacters-in-image", func(t *testing.T) {
		spec := DeploymentSpec{
			Containers: []ContainerSpec{
				{Name: "myapp", Image: "nginx;rm -rf /"},
			},
		}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for shell metacharacters in image")
		}
	})

	t.Run("shell-metacharacters-in-tag", func(t *testing.T) {
		spec := DeploymentSpec{
			Containers: []ContainerSpec{
				{Name: "myapp", Image: "nginx", Tag: "latest|evil"},
			},
		}
		err := spec.Validate()
		if err == nil {
			t.Error("expected error for shell metacharacters in tag")
		}
	})
}

func TestHealthCheckUnmarshalJSON(t *testing.T) {
	t.Run("array-format", func(t *testing.T) {
		data := `{"test":["CMD-SHELL","curl -f http://localhost"],"interval":"10s","retries":3}`
		var hc HealthCheck
		if err := json.Unmarshal([]byte(data), &hc); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(hc.Test) != 2 || hc.Test[0] != "CMD-SHELL" {
			t.Errorf("test = %v, want [CMD-SHELL, curl ...]", hc.Test)
		}
		if hc.Interval != "10s" {
			t.Errorf("interval = %q, want %q", hc.Interval, "10s")
		}
		if hc.Retries != 3 {
			t.Errorf("retries = %d, want 3", hc.Retries)
		}
	})

	t.Run("string-format", func(t *testing.T) {
		data := `{"test":"curl -f http://localhost","interval":"5s"}`
		var hc HealthCheck
		if err := json.Unmarshal([]byte(data), &hc); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(hc.Test) != 2 || hc.Test[0] != "CMD-SHELL" || hc.Test[1] != "curl -f http://localhost" {
			t.Errorf("test = %v, want [CMD-SHELL, curl -f http://localhost]", hc.Test)
		}
	})

	t.Run("empty-test", func(t *testing.T) {
		data := `{"interval":"10s","retries":3}`
		var hc HealthCheck
		if err := json.Unmarshal([]byte(data), &hc); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if hc.Test != nil {
			t.Errorf("test = %v, want nil", hc.Test)
		}
	})

	t.Run("null-test", func(t *testing.T) {
		data := `{"test":null,"interval":"10s"}`
		var hc HealthCheck
		if err := json.Unmarshal([]byte(data), &hc); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if hc.Test != nil {
			t.Errorf("test = %v, want nil", hc.Test)
		}
	})
}

// TestCleanupDecisionNeverRemovesDatabases guards the rule whose violation
// destroys data rather than merely failing a deploy.
//
// Managed database containers carry managed-by=lattice and no lattice-stack
// label, so before this guard existed the port-conflict fallback would stop and
// remove a live database to hand its host port to a deploying stack — and an
// empty stack name would match every database via the stack check.
func TestCleanupDecisionNeverRemovesDatabases(t *testing.T) {
	dbLabels := map[string]string{
		"managed-by":     "lattice",
		"lattice-type":   "database",
		"lattice-engine": "mariadb",
	}

	tests := []struct {
		name        string
		stackName   string
		publicPorts []uint16
		neededPorts map[string]bool
	}{
		{
			name:        "database holding a port the new spec wants",
			stackName:   "some-stack",
			publicPorts: []uint16{20000},
			neededPorts: map[string]bool{"20000": true},
		},
		{
			name:        "empty stack name must not match the absent stack label",
			stackName:   "",
			publicPorts: nil,
			neededPorts: nil,
		},
		{
			name:        "database with no ports and an unrelated stack",
			stackName:   "other",
			publicPorts: nil,
			neededPorts: map[string]bool{"8080": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remove, reason := cleanupDecision(
				dbLabels, "lattice-db-testdb", tt.publicPorts,
				tt.stackName, map[string]bool{}, tt.neededPorts,
			)
			if remove {
				t.Errorf("cleanup would remove a managed database (%s) — this is data loss", reason)
			}
		})
	}
}

func TestCleanupDecisionSkipsForeignContainers(t *testing.T) {
	foreign := map[string]string{"com.docker.compose.project": "something-else"}
	remove, reason := cleanupDecision(
		foreign, "postgres", []uint16{5432},
		"stack", map[string]bool{}, map[string]bool{"5432": true},
	)
	if remove {
		t.Errorf("cleanup would remove a foreign container (%s); failing the port bind is correct instead", reason)
	}
}

func TestCleanupDecisionRemovesStaleStackContainers(t *testing.T) {
	labels := map[string]string{
		"managed-by":    "lattice",
		"lattice-stack": "my-stack",
	}

	t.Run("stale container from this stack is removed", func(t *testing.T) {
		remove, reason := cleanupDecision(
			labels, "old-service", nil,
			"my-stack", map[string]bool{"new-service": true}, nil,
		)
		if !remove {
			t.Error("a stack container absent from the new spec should be removed")
		}
		if reason == "" {
			t.Error("removal must carry a reason")
		}
	})

	t.Run("container still in the spec is kept", func(t *testing.T) {
		remove, _ := cleanupDecision(
			labels, "kept-service", nil,
			"my-stack", map[string]bool{"kept-service": true}, nil,
		)
		if remove {
			t.Error("a container present in the new spec must not be removed")
		}
	})

	t.Run("lattice container holding a needed port is reclaimed", func(t *testing.T) {
		legacy := map[string]string{"managed-by": "lattice"}
		remove, _ := cleanupDecision(
			legacy, "legacy", []uint16{8080},
			"my-stack", map[string]bool{}, map[string]bool{"8080": true},
		)
		if !remove {
			t.Error("a Lattice-managed non-database container holding a needed port should be reclaimed")
		}
	})
}

func TestShouldProbe(t *testing.T) {
	tests := []struct {
		name                          string
		ports, oldContainers, volumes int
		want                          bool
	}{
		{"ports and predecessor, no volumes", 2, 1, 0, true},
		{"stateful container with volumes", 2, 1, 2, false},
		{"no host ports", 0, 1, 0, false},
		{"first deploy, no predecessor", 2, 0, 0, false},
		{"volumes without ports", 0, 1, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldProbe(tt.ports, tt.oldContainers, tt.volumes); got != tt.want {
				t.Errorf("shouldProbe(%d, %d, %d) = %v, want %v", tt.ports, tt.oldContainers, tt.volumes, got, tt.want)
			}
		})
	}
}

func TestNeedsGreen(t *testing.T) {
	tests := []struct {
		name string
		spec ContainerSpec
		want bool
	}{
		{"stateless", ContainerSpec{Name: "web"}, true},
		{"stateful", ContainerSpec{Name: "db", Volumes: map[string]string{"db-data": "/var/lib/mysql"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsGreen(tt.spec); got != tt.want {
				t.Errorf("needsGreen(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The API sends ulimits as [{name, soft, hard}] (lattice-api specBuilder);
// this pins the wire contract and the conversion to the docker spec.
func TestContainerSpecUlimitsContract(t *testing.T) {
	raw := `{"name":"monitor-clickhouse","image":"clickhouse/clickhouse-server","ulimits":[{"name":"nofile","soft":262144,"hard":262144},{"name":"memlock","soft":-1,"hard":-1}]}`
	var c ContainerSpec
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	got := convertUlimits(c.Ulimits)
	if len(got) != 2 || got[0].Name != "nofile" || got[0].Soft != 262144 || got[0].Hard != 262144 || got[1].Soft != -1 {
		t.Errorf("convertUlimits = %+v", got)
	}
	if convertUlimits(nil) != nil {
		t.Error("no ulimits must stay nil so the daemon defaults apply")
	}
}
