package gargle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
)

type Tagger struct {
	client      *artifactregistry.Client
	knownImages *imageList
	tagPrefix   string
	log         *logrus.Logger
}

func NewTagger(ctx context.Context, log *logrus.Logger, client *artifactregistry.Client, envName string, knownImages *imageList) *Tagger {
	return &Tagger{
		client:      client,
		knownImages: knownImages,
		tagPrefix:   "keep-nais-" + envName + "-",
		log:         log,
	}
}

func (t *Tagger) Close() error {
	return t.client.Close()
}

func (t *Tagger) Run(ctx context.Context, repos Repositories) error {
	wg, grpCtx := errgroup.WithContext(ctx)
	wg.SetLimit(5)
	for _, r := range repos {
		wg.Go(func() error {
			t.log.Debugf("Cleaning and tagging registry %q", r.Name)
			if err := t.cleanRepository(grpCtx, r.Name); err != nil {
				return fmt.Errorf("failed to clean repository: %w", err)
			}
			return t.TagRegistry(grpCtx, r)
		})
	}

	return wg.Wait()
}

func (t *Tagger) cleanRepository(ctx context.Context, repository string) error {
	iter := t.client.ListDockerImages(ctx, &artifactregistrypb.ListDockerImagesRequest{
		Parent: repository,
	})

OUTER:
	for {
		resp, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				return nil
			}
			return fmt.Errorf("failed to list docker images: %w", err)
		}

		// Ignore sig and att images
		for _, tag := range resp.Tags {
			if !strings.HasPrefix(tag, t.tagPrefix) && (strings.HasSuffix(tag, ".sig") || strings.HasSuffix(tag, ".att")) {
				continue OUTER
			}

			uriName, _, _ := strings.Cut(resp.Uri, "@")
			if t.knownImages.HasImage(uriName, tag) {
				continue OUTER
			}
		}

		for _, tag := range resp.Tags {
			if strings.HasPrefix(tag, t.tagPrefix) {
				name, _, found := strings.Cut(resp.Name, "@")
				if !found {
					continue
				}

				// Untag the image
				if err := t.UntagImage(ctx, name, tag); err != nil {
					return fmt.Errorf("failed to untag image: %w", err)
				}

				// Untag tag.sig and tag.att
				if err := t.UntagImage(ctx, name, tag+".sig"); err != nil && !notFoundErr(err) {
					return fmt.Errorf("failed to untag image: %w", err)
				}

				if err := t.UntagImage(ctx, name, tag+".att"); err != nil && !notFoundErr(err) {
					return fmt.Errorf("failed to untag image: %w", err)
				}
			}
		}
	}
}

func (t *Tagger) TagRegistry(ctx context.Context, reg Repository) error {
	images := t.knownImages.ForPrefix(reg.URL)
	for name, tags := range images {
		for _, tag := range tags {
			if err := t.KeepImage(ctx, reg, name, tag); err != nil {
				return err
			}
		}
	}

	return nil
}

func (t *Tagger) KeepImage(ctx context.Context, reg Repository, name, tag string) error {
	// Base image
	version, keepTag, err := t.TagImage(ctx, reg, name, tag, "")
	if err != nil {
		if notFoundErr(err) {
			return nil
		}
		t.log.Debugf("base image: %v", err)
	}

	// Tag sig and att images
	if _, _, err := t.TagImage(ctx, reg, name, version+".sig", keepTag+".sig"); err != nil {
		if notFoundErr(err) {
			return nil
		}
		t.log.Debugf("sig image: %v", err)
	}

	if _, _, err := t.TagImage(ctx, reg, name, version+".att", keepTag+".att"); err != nil {
		if notFoundErr(err) {
			return nil
		}
		t.log.Debugf("att image: %v", err)
	}

	return nil
}

func (t *Tagger) TagImage(ctx context.Context, reg Repository, name, tag, keepTag string) (string, string, error) {
	pkg := strings.Trim(strings.ReplaceAll(name, reg.URL, ""), "/")

	image, err := t.client.GetTag(ctx, &artifactregistrypb.GetTagRequest{
		Name: reg.Tag(pkg, tag),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to get docker image %q: %w", reg.Tag(pkg, tag), err)
	}

	version := ""
	if keepTag == "" {
		versionParts := strings.Split(image.Version, "/")
		version = strings.ReplaceAll(versionParts[len(versionParts)-1], ":", "-")

		keepTag = (t.tagPrefix + version)
	}

	// Tag the image
	return version, keepTag, t.ApplyImageTag(ctx, reg, image.Version, pkg, keepTag)
}

func (t *Tagger) UntagImage(ctx context.Context, name, tag string) error {
	t.log.Debugf("Untagging %q", tag)
	tagPrefix := strings.Replace(name, "/dockerImages/", "/packages/", 1) + "/tags/"

	err := t.client.DeleteTag(ctx, &artifactregistrypb.DeleteTagRequest{
		Name: tagPrefix + tag,
	})
	if err != nil && !notFoundErr(err) {
		return fmt.Errorf("untagging %q: %w", tagPrefix+tag, err)
	}
	return nil
}

func (t *Tagger) ApplyImageTag(ctx context.Context, reg Repository, version, pkg, tag string) error {
	t.log.Debugf("Tagging %q with %q", reg.Tag(pkg, tag), version)
	_, err := t.client.CreateTag(ctx, &artifactregistrypb.CreateTagRequest{
		Parent: reg.Image(pkg),
		TagId:  tag,
		Tag: &artifactregistrypb.Tag{
			Name:    reg.Tag(pkg, tag),
			Version: version,
		},
	})
	if err != nil && !alreadyExistsErr(err) {
		return fmt.Errorf("failed to create tag for %q: %w", reg.Tag(pkg, tag), err)
	}
	return nil
}

func notFoundErr(err error) bool {
	if apiErr, ok := apierror.FromError(err); ok {
		if apiErr.GRPCStatus() == nil {
			return apiErr.HTTPCode() == 404
		}
		return apiErr.GRPCStatus().Code() == codes.NotFound
	}
	return false
}

func alreadyExistsErr(err error) bool {
	if apiErr, ok := apierror.FromError(err); ok {
		if apiErr.GRPCStatus() == nil {
			return apiErr.HTTPCode() == 409
		}
		return apiErr.GRPCStatus().Code() == codes.AlreadyExists
	}
	return false
}
