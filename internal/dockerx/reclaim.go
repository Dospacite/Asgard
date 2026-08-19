package dockerx

import (
	"context"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// Image is the subset of a Docker image summary Asgard reasons about when
// deciding what it is safe to reclaim.
type Image struct {
	ID         string            `json:"id"`
	Tags       []string          `json:"tags"`
	CreatedAt  time.Time         `json:"createdAt"`
	SizeBytes  int64             `json:"sizeBytes"`
	Containers int64             `json:"containers"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// Dangling reports an image left with no tag at all. Every rebuild of a service
// whose tag already existed leaves the previous layers behind like this.
func (i Image) Dangling() bool {
	if len(i.Tags) == 0 {
		return true
	}
	for _, tag := range i.Tags {
		if tag != "<none>:<none>" && tag != "<none>" {
			return false
		}
	}
	return true
}

// Images lists the images the daemon holds.
func (e *Engine) Images(ctx context.Context) ([]Image, error) {
	result, err := e.client.ImageList(ctx, client.ImageListOptions{All: false})
	if err != nil {
		return nil, err
	}
	images := make([]Image, 0, len(result.Items))
	for _, item := range result.Items {
		tags := []string{}
		for _, tag := range item.RepoTags {
			if tag != "" && tag != "<none>:<none>" {
				tags = append(tags, tag)
			}
		}
		images = append(images, Image{
			ID:         item.ID,
			Tags:       tags,
			CreatedAt:  time.Unix(item.Created, 0).UTC(),
			SizeBytes:  item.Size,
			Containers: item.Containers,
			Labels:     item.Labels,
		})
	}
	return images, nil
}

// RemoveImage deletes one image reference.
//
// A tag is removed by name rather than by ID on purpose: an ID removal drops
// every tag pointing at that image, and two releases whose build produced
// identical layers share one ID. Removing by tag untags exactly the release
// being reclaimed and lets the daemon free the layers once the last tag goes.
func (e *Engine) RemoveImage(ctx context.Context, ref string) error {
	_, err := e.client.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	return err
}

// PruneBuildCache trims the builder cache down to a budget.
//
// The cache is pure derived data — it makes the next build faster and holds no
// state — but nothing bounds it, so it grows until the disk is the limit. A
// budget keeps the speed benefit for recent builds while capping the cost;
// pruning it to nothing would make every subsequent build a cold one.
func (e *Engine) PruneBuildCache(ctx context.Context, keepBytes int64) (int64, error) {
	if keepBytes < 0 {
		keepBytes = 0
	}
	// MaxUsedSpace caps total cache usage; the daemon evicts least-recently-used
	// entries down to it. All:false leaves cache still referenced by an image
	// alone, so a rebuild of an unchanged layer stays a cache hit.
	result, err := e.client.BuildCachePrune(ctx, client.BuildCachePruneOptions{MaxUsedSpace: keepBytes})
	if err != nil {
		return 0, err
	}
	return int64(result.Report.SpaceReclaimed), nil
}

// RemoveContainer deletes a container by ID, tolerating one that has already
// gone away.
func (e *Engine) RemoveContainer(ctx context.Context, id string) error {
	err := e.Remove(ctx, id, false)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return nil
	}
	return err
}

// DiskUsage is what the daemon reports for its own storage.
type DiskUsage struct {
	ImagesBytes           int64 `json:"imagesBytes"`
	ImagesReclaimable     int64 `json:"imagesReclaimableBytes"`
	ContainersBytes       int64 `json:"containersBytes"`
	BuildCacheBytes       int64 `json:"buildCacheBytes"`
	BuildCacheReclaimable int64 `json:"buildCacheReclaimableBytes"`
	VolumesBytes          int64 `json:"volumesBytes"`
}

// Total is everything Docker is holding.
func (d DiskUsage) Total() int64 {
	return d.ImagesBytes + d.ContainersBytes + d.BuildCacheBytes + d.VolumesBytes
}

// DiskUsage asks the daemon how much space it is using.
//
// This is the only honest way to report what a reclamation actually freed.
// Summing the size of each removed image double-counts every layer they share
// — and release images of one service share almost all of theirs — which
// overstates the result by a factor of two or more.
func (e *Engine) DiskUsage(ctx context.Context) (DiskUsage, error) {
	result, err := e.client.DiskUsage(ctx, client.DiskUsageOptions{Containers: true, Images: true, BuildCache: true, Volumes: true})
	if err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{
		ImagesBytes:           result.Images.TotalSize,
		ImagesReclaimable:     result.Images.Reclaimable,
		ContainersBytes:       result.Containers.TotalSize,
		BuildCacheBytes:       result.BuildCache.TotalSize,
		BuildCacheReclaimable: result.BuildCache.Reclaimable,
		VolumesBytes:          result.Volumes.TotalSize,
	}, nil
}
