package deploy

import (
	"reflect"
	"sort"
	"testing"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

func TestImageRepo(t *testing.T) {
	cases := map[string]string{
		"nginx":                                   "nginx",
		"nginx:1.27":                              "nginx",
		"registry.appleby.cloud/app:latest":       "registry.appleby.cloud/app",
		"registry:5000/app":                       "registry:5000/app",
		"registry:5000/app:v2":                    "registry:5000/app",
		"registry.appleby.cloud/app@sha256:abc":   "registry.appleby.cloud/app",
		"registry:5000/team/app:v1@sha256:abcdef": "registry:5000/team/app",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := imageRepo(in); got != want {
				t.Errorf("imageRepo(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestPruneSettings(t *testing.T) {
	cases := []struct {
		name        string
		in          *ImagePruneSpec
		wantEnabled bool
		wantKeep    int
	}{
		{"absent defaults on, keep 2", nil, true, DEFAULT_IMAGE_KEEP},
		{"disabled", &ImagePruneSpec{Enabled: false, Keep: 2}, false, 2},
		{"keep 5", &ImagePruneSpec{Enabled: true, Keep: 5}, true, 5},
		{"negative keep clamps to 0", &ImagePruneSpec{Enabled: true, Keep: -3}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, k := pruneSettings(c.in)
			if e != c.wantEnabled || k != c.wantKeep {
				t.Errorf("pruneSettings = (%v, %d), want (%v, %d)", e, k, c.wantEnabled, c.wantKeep)
			}
		})
	}
}

const app = "registry.appleby.cloud/app"

// old is a previous :latest of app that only keeps its digest.
func old(id string, created int64) dockerclient.ImageInfo {
	return dockerclient.ImageInfo{ID: id, RepoDigests: []string{app + "@sha256:" + id}, Created: created, Size: 100}
}

func ids(ims []dockerclient.ImageInfo) []string {
	out := []string{}
	for _, im := range ims {
		out = append(out, im.ID)
	}
	sort.Strings(out)
	return out
}

func TestSelectPrune(t *testing.T) {
	current := dockerclient.ImageInfo{ID: "cur", RepoTags: []string{app + ":latest"}, RepoDigests: []string{app + "@sha256:cur"}, Created: 100}
	history := []dockerclient.ImageInfo{current, old("o1", 90), old("o2", 80), old("o3", 70), old("o4", 60)}
	repos := map[string]bool{app: true}

	cases := []struct {
		name   string
		images []dockerclient.ImageInfo
		inUse  map[string]bool
		keep   int
		want   []string
	}{
		{
			name:   "keeps the newest 2 previous, removes the rest",
			images: history,
			inUse:  map[string]bool{"cur": true},
			keep:   2,
			want:   []string{"o3", "o4"},
		},
		{
			name:   "in-use image counts toward nothing and is never removed",
			images: history,
			inUse:  map[string]bool{"cur": true, "o4": true},
			keep:   2,
			want:   []string{"o3"},
		},
		{
			name:   "keep 0 removes every unused image of the repo",
			images: history,
			inUse:  map[string]bool{"cur": true},
			keep:   0,
			want:   []string{"o1", "o2", "o3", "o4"},
		},
		{
			name: "other stacks' repositories are untouched",
			images: append(history,
				dockerclient.ImageInfo{ID: "other1", RepoDigests: []string{"registry.appleby.cloud/other@sha256:x"}, Created: 1},
				dockerclient.ImageInfo{ID: "other2", RepoDigests: []string{"registry.appleby.cloud/other@sha256:y"}, Created: 2},
			),
			inUse: map[string]bool{"cur": true},
			keep:  0,
			want:  []string{"o1", "o2", "o3", "o4"},
		},
		{
			name: "database and base images are untouched",
			images: append(history,
				dockerclient.ImageInfo{ID: "maria", RepoTags: []string{"mariadb:11"}, Created: 1},
				dockerclient.ImageInfo{ID: "alpine", RepoTags: []string{"alpine:3.24"}, Created: 1},
			),
			inUse: map[string]bool{"cur": true},
			keep:  2,
			want:  []string{"o3", "o4"},
		},
		{
			name: "image shared with another repository is kept",
			images: []dockerclient.ImageInfo{
				current,
				{ID: "shared", RepoTags: []string{"registry.appleby.cloud/other:v1"}, RepoDigests: []string{app + "@sha256:s"}, Created: 1},
			},
			inUse: map[string]bool{"cur": true},
			keep:  0,
			want:  []string{},
		},
		{
			name:   "untagged image with no digest is not ours to judge",
			images: []dockerclient.ImageInfo{current, {ID: "local", Created: 1}},
			inUse:  map[string]bool{"cur": true},
			keep:   0,
			want:   []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ids(selectPrune(c.images, c.inUse, repos, c.keep))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("selectPrune = %v, want %v", got, c.want)
			}
		})
	}
}
