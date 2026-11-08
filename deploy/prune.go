package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// DEFAULT_IMAGE_KEEP is how many previous images per repository survive a
// post-deploy prune when the orchestrator doesn't say (an older lattice-api),
// so a rollback can start without a pull.
const DEFAULT_IMAGE_KEEP = 2

// PRUNE_TIMEOUT bounds the post-deploy prune; it never holds up a deploy.
const PRUNE_TIMEOUT = 2 * time.Minute

// ImagePruneSpec is the stack's prune setting from the deploy spec. Absent
// means enabled with DEFAULT_IMAGE_KEEP.
type ImagePruneSpec struct {
	Enabled bool `json:"enabled"`
	Keep    int  `json:"keep"`
}

// pruneSettings resolves the spec's setting with defaults.
func pruneSettings(p *ImagePruneSpec) (enabled bool, keep int) {
	if p == nil {
		return true, DEFAULT_IMAGE_KEEP
	}
	if p.Keep < 0 {
		return p.Enabled, 0
	}
	return p.Enabled, p.Keep
}

// imageRepo strips the tag and digest from an image reference:
// "registry:5000/app:v1" and "registry:5000/app@sha256:…" are both
// "registry:5000/app". A colon before the last slash is a registry port.
func imageRepo(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// imageRepos is every repository an image is known under, from its tags and
// its digests (an image replaced by a newer :latest keeps only its digest).
func imageRepos(im dockerclient.ImageInfo) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range append(append([]string{}, im.RepoTags...), im.RepoDigests...) {
		if r == "" || strings.HasPrefix(r, "<none>") {
			continue
		}
		repo := imageRepo(r)
		if !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	return out
}

// selectPrune picks the images to remove after a deploy: images of the
// deployed stack's repositories (repos) that no container uses (inUse holds
// image IDs of every container on the worker, any state), beyond the newest
// keep per repository. Images of other repositories — other stacks, managed
// databases, base images — are never selected, nor is an image that also
// belongs to a repository outside repos.
func selectPrune(images []dockerclient.ImageInfo, inUse map[string]bool, repos map[string]bool, keep int) []dockerclient.ImageInfo {
	byRepo := map[string][]dockerclient.ImageInfo{}
	for _, im := range images {
		if inUse[im.ID] {
			continue
		}
		rs := imageRepos(im)
		if len(rs) == 0 {
			continue
		}
		ours := true
		for _, r := range rs {
			if !repos[r] {
				ours = false
				break
			}
		}
		if !ours {
			continue
		}
		byRepo[rs[0]] = append(byRepo[rs[0]], im)
	}
	var out []dockerclient.ImageInfo
	for _, ims := range byRepo {
		sort.Slice(ims, func(i, j int) bool {
			if ims[i].Created != ims[j].Created {
				return ims[i].Created > ims[j].Created
			}
			return ims[i].ID < ims[j].ID
		})
		if len(ims) > keep {
			out = append(out, ims[keep:]...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// pruneImages removes the deployed stack's unused old images after a
// successful deploy and reports what it reclaimed in the deployment log. It is
// best-effort: an error is logged and reported, never returned.
func (e *Executor) pruneImages(ctx context.Context, spec DeploymentSpec) {
	enabled, keep := pruneSettings(spec.ImagePrune)
	if !enabled || e.Docker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PRUNE_TIMEOUT)
	defer cancel()
	fail := func(step string, err error) {
		slog.WarnContext(ctx, "post-deploy image prune failed", "component", "deploy", "deployment_id", spec.DeploymentID,
			"stack", spec.StackName, "step", step, "error", err)
		e.reportProgress(spec.DeploymentID, "validating", fmt.Sprintf("image prune skipped (%s failed: %v); deployment unaffected", step, err),
			map[string]any{"step": "image_prune"})
	}

	repos := map[string]bool{}
	for _, c := range spec.Containers {
		repos[imageRepo(c.Image)] = true
	}
	containers, err := e.Docker.ListContainers(ctx, "")
	if err != nil {
		fail("list containers", err)
		return
	}
	inUse := map[string]bool{}
	for _, c := range containers {
		inUse[c.ImageID] = true
	}
	images, err := e.Docker.ListImages(ctx)
	if err != nil {
		fail("list images", err)
		return
	}

	var removed, failed int
	var reclaimed int64
	for _, im := range selectPrune(images, inUse, repos, keep) {
		if err := e.Docker.RemoveImage(ctx, im.ID); err != nil {
			failed++
			slog.WarnContext(ctx, "post-deploy image remove failed", "component", "deploy", "deployment_id", spec.DeploymentID,
				"stack", spec.StackName, "image_id", im.ID, "error", err)
			continue
		}
		removed++
		reclaimed += im.Size
	}
	msg := fmt.Sprintf("image prune: removed %d unused image(s), reclaimed %.1f MB (kept %d previous per repository)",
		removed, float64(reclaimed)/1024/1024, keep)
	if failed > 0 {
		msg += fmt.Sprintf(", %d could not be removed", failed)
	}
	slog.InfoContext(ctx, "post-deploy image prune", "component", "deploy", "deployment_id", spec.DeploymentID, "stack", spec.StackName,
		"images_removed", removed, "images_failed", failed, "bytes_reclaimed", reclaimed, "keep", keep)
	e.reportProgress(spec.DeploymentID, "validating", msg, map[string]any{
		"step": "image_prune", "images_removed": removed, "bytes_reclaimed": reclaimed,
	})
}
