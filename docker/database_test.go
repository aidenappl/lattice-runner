package docker

import "testing"

func TestDefaultDBPort(t *testing.T) {
	tests := []struct {
		name   string
		engine string
		want   int
	}{
		{"mysql", "mysql", 3306},
		{"mariadb", "mariadb", 3306},
		{"postgres", "postgres", 5432},
		{"unknown-defaults-to-3306", "mongodb", 3306},
		{"empty-defaults-to-3306", "", 3306},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultDBPort(tt.engine)
			if got != tt.want {
				t.Errorf("defaultDBPort(%q) = %d, want %d", tt.engine, got, tt.want)
			}
		})
	}
}

func TestCreateDatabaseContainerValidation(t *testing.T) {
	// CreateDatabaseContainer is a method on *Client, but validation happens
	// before any Docker API calls. We create a Client with a nil underlying
	// Docker client — the validation should fail before it's ever used.
	c := &Client{} // cli is nil, but validation errors occur first

	tests := []struct {
		name    string
		spec    DatabaseSpec
		wantErr string
	}{
		{
			"missing-engine",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "vol1",
				Engine:        "",
				EngineVersion: "8.0",
				Port:          3306,
				DatabaseName:  "mydb",
				Username:      "user",
			},
			"engine and engine_version are required",
		},
		{
			"missing-engine-version",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "vol1",
				Engine:        "mysql",
				EngineVersion: "",
				Port:          3306,
				DatabaseName:  "mydb",
				Username:      "user",
			},
			"engine and engine_version are required",
		},
		{
			"missing-container-name",
			DatabaseSpec{
				ContainerName: "",
				VolumeName:    "vol1",
				Engine:        "mysql",
				EngineVersion: "8.0",
				Port:          3306,
				DatabaseName:  "mydb",
				Username:      "user",
			},
			"container_name and volume_name are required",
		},
		{
			"missing-volume-name",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "",
				Engine:        "mysql",
				EngineVersion: "8.0",
				Port:          3306,
				DatabaseName:  "mydb",
				Username:      "user",
			},
			"container_name and volume_name are required",
		},
		{
			"port-zero",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "vol1",
				Engine:        "mysql",
				EngineVersion: "8.0",
				Port:          0,
				DatabaseName:  "mydb",
				Username:      "user",
			},
			"port must be positive",
		},
		{
			"port-negative",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "vol1",
				Engine:        "mysql",
				EngineVersion: "8.0",
				Port:          -1,
				DatabaseName:  "mydb",
				Username:      "user",
			},
			"port must be positive",
		},
		{
			"missing-database-name",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "vol1",
				Engine:        "mysql",
				EngineVersion: "8.0",
				Port:          3306,
				DatabaseName:  "",
				Username:      "user",
			},
			"database_name and username are required",
		},
		{
			"missing-username",
			DatabaseSpec{
				ContainerName: "db1",
				VolumeName:    "vol1",
				Engine:        "mysql",
				EngineVersion: "8.0",
				Port:          3306,
				DatabaseName:  "mydb",
				Username:      "",
			},
			"database_name and username are required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateDatabaseContainer(t.Context(), tt.spec)
			if err == nil {
				t.Fatal("expected error but got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}
