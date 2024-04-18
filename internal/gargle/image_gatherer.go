package gargle

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

type imageList struct {
	lock sync.RWMutex

	// list is the list of images with their tags
	list map[string][]string
}

func (i *imageList) AddImage(image string) {
	if strings.HasPrefix(image, "-") {
		return
	}
	// For testing, only accept these images:
	name, tag, found := strings.Cut(image, ":")
	if !found {
		return
	}

	i.lock.Lock()
	defer i.lock.Unlock()

	tags := append(i.list[name], tag)
	slices.Sort(tags)
	i.list[name] = slices.Compact(tags)
}

func (i *imageList) HasImage(name, tag string) bool {
	i.lock.RLock()
	defer i.lock.RUnlock()

	tags, ok := i.list[name]
	if !ok {
		return false
	}

	return slices.Contains(tags, tag)
}

func (t *imageList) ForPrefix(prefix string) map[string][]string {
	t.lock.RLock()
	defer t.lock.RUnlock()

	m := make(map[string][]string)
	for name, tags := range t.list {
		if strings.HasPrefix(name, prefix) {
			m[name] = tags
		}
	}

	return m
}

type ImageGatherer struct {
	imagesFrom ImagesFrom
	imageList  *imageList
}

func NewImageGatherer(imagesFrom ImagesFrom) *ImageGatherer {
	return &ImageGatherer{
		imageList: &imageList{
			list: make(map[string][]string),
		},
		imagesFrom: imagesFrom,
	}
}

func (i *ImageGatherer) Gather(ctx context.Context, client *dynamic.DynamicClient, prefixes []string) error {
	grp, ctx := errgroup.WithContext(ctx)
	grp.SetLimit(5)
	for groupVersion, paths := range i.imagesFrom {
		for _, path := range paths {
			grp.Go(func() error {
				return i.gatherResource(ctx, client, groupVersion, path, prefixes)
			})
		}
	}

	return grp.Wait()
}

func (i *ImageGatherer) gatherResource(ctx context.Context, client *dynamic.DynamicClient, groupVersion GroupVersion, path ResourcePath, prefixes []string) error {
	gvr := groupVersion.WithResource(path.Resource)
	list, err := client.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list resources: %w", err)
	}

	return list.EachListItem(func(o runtime.Object) error {
		u := o.(*unstructured.Unstructured)
		res := path.JSONPath.Get(u.Object)

		for _, image := range res {
			img, ok := image.(string)
			if !ok {
				continue
			}

			found := false
			for _, prefix := range prefixes {
				if strings.HasPrefix(img, prefix) {
					found = true
					break
				}
			}

			if !found {
				continue
			}

			i.imageList.AddImage(img)
		}

		return nil
	})
}
