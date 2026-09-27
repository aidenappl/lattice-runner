package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

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

// fakeDockerLogs serves the container logs endpoint with a fixed status, so the
// real Docker SDK error mapping (errdefs) is exercised end to end.
func fakeDockerLogs(t *testing.T, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"No such container: 0a61bb2068c9"}`))
	}))
	t.Cleanup(srv.Close)

	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")),
		client.WithHTTPClient(srv.Client()),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	return &Client{cli: cli}
}

func TestDoStreamReportsGoneContainer(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantGone bool
	}{
		{"removed container", http.StatusNotFound, true},
		{"daemon error is retryable", http.StatusInternalServerError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ls := NewLogStreamer(fakeDockerLogs(t, tt.status), func(LogLine) {}, time.Hour)
			_, gone := ls.doStream(context.Background(), "0a61bb2068c9", "trailblaze-auth-v2", time.Time{})
			if gone != tt.wantGone {
				t.Errorf("gone = %v, want %v", gone, tt.wantGone)
			}
		})
	}
}

// A removed container must end its stream goroutine immediately rather than
// backing off and retrying an ID that can never come back.
func TestStreamExitsWhenContainerRemoved(t *testing.T) {
	ls := NewLogStreamer(fakeDockerLogs(t, http.StatusNotFound), func(LogLine) {}, time.Hour)
	done := make(chan struct{})
	go ls.stream(context.Background(), "0a61bb2068c9", "trailblaze-auth-v2", done)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stream kept retrying a removed container (first backoff is 1s)")
	}
}
