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
	"google.golang.org/api/googleapi"
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
	var wg errgroup.Group
	wg.SetLimit(5)
	repoErrors := make([]error, len(repos))
	for index, r := range repos {
		wg.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			t.log.Debugf("Cleaning and tagging registry %q", r.Name)
			if err := t.cleanRepository(ctx, r.Name); err != nil {
				t.log.WithField("repository", r.Name).WithError(err).Error("Failed to clean repository")
				repoErrors[index] = fmt.Errorf("failed to clean repository %q: %w", r.Name, err)
			}
			if err := t.TagRegistry(ctx, r); err != nil {
				repoErrors[index] = errors.Join(repoErrors[index], fmt.Errorf("failed to tag repository %q: %w", r.Name, err))
			}
			return nil
		})
	}

	return errors.Join(wg.Wait(), errors.Join(repoErrors...))
}

func (t *Tagger) cleanRepository(ctx context.Context, repository string) error {
	iter := t.client.ListDockerImages(ctx, &artifactregistrypb.ListDockerImagesRequest{
		Parent: repository,
	})

	var imageErrors []error
OUTER:
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(imageErrors...), err)
		}
		resp, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				return errors.Join(imageErrors...)
			}
			return errors.Join(errors.Join(imageErrors...), fmt.Errorf("failed to list docker images: %w", err))
		}

		uriName, digest, _ := strings.Cut(resp.Uri, "@")
		if t.knownImages.HasImage(uriName, digest) {
			continue
		}

		// Ignore sig and att images
		for _, tag := range resp.Tags {
			if !strings.HasPrefix(tag, t.tagPrefix) && (strings.HasSuffix(tag, ".sig") || strings.HasSuffix(tag, ".att")) {
				continue OUTER
			}

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

				for _, suffix := range []string{"", ".sig", ".att"} {
					if err := ctx.Err(); err != nil {
						return errors.Join(errors.Join(imageErrors...), err)
					}
					if err := t.UntagImage(ctx, name, tag+suffix); err != nil {
						t.log.WithFields(logrus.Fields{
							"repository": repository,
							"image":      name,
							"tag":        tag + suffix,
						}).WithError(err).Error("Failed to untag image")
						imageErrors = append(imageErrors, err)
					}
				}
			}
		}
	}
}

func (t *Tagger) TagRegistry(ctx context.Context, reg Repository) error {
	images := t.knownImages.ForPrefix(reg.URL)
	var imageErrors []error
	for name, tags := range images {
		for _, tag := range tags {
			if err := ctx.Err(); err != nil {
				return errors.Join(errors.Join(imageErrors...), err)
			}
			if err := t.KeepImage(ctx, reg, name, tag); err != nil {
				t.log.WithFields(logrus.Fields{
					"repository": reg.Name,
					"image":      name,
					"reference":  tag,
				}).WithError(err).Error("Failed to keep image")
				imageErrors = append(imageErrors, fmt.Errorf("image %q at %q: %w", name, tag, err))
			}
		}
	}

	return errors.Join(imageErrors...)
}

func (t *Tagger) KeepImage(ctx context.Context, reg Repository, name, tag string) error {
	// Base image
	version, keepTag, err := t.TagImage(ctx, reg, name, tag, "")
	if err != nil {
		if notFoundErr(err) || missingVersionErr(err) {
			t.log.WithFields(logrus.Fields{
				"image":     name,
				"reference": tag,
			}).WithError(err).Warn("Referenced image not found; skipping")
			return nil
		}
		return fmt.Errorf("base image: %w", err)
	}

	// Tag sig and att images
	var imageErrors []error
	for _, suffix := range []string{".sig", ".att"} {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(imageErrors...), err)
		}
		if _, _, err := t.TagImage(ctx, reg, name, version+suffix, keepTag+suffix); err != nil && !notFoundErr(err) && !missingVersionErr(err) {
			imageErrors = append(imageErrors, fmt.Errorf("%s image: %w", suffix[1:], err))
		}
	}

	return errors.Join(imageErrors...)
}

func (t *Tagger) TagImage(ctx context.Context, reg Repository, name, tag, keepTag string) (string, string, error) {
	pkg := strings.TrimPrefix(name, reg.URL+"/")

	imageVersion := ""
	if keepTag == "" && strings.Contains(tag, ":") {
		// Pinned digests are authoritative, even if the accompanying tag has moved.
		imageVersion = reg.Image(pkg) + "/versions/" + tag
	} else {
		image, err := t.client.GetTag(ctx, &artifactregistrypb.GetTagRequest{
			Name: reg.Tag(pkg, tag),
		})
		if err != nil {
			return "", "", fmt.Errorf("failed to get docker image %q: %w", reg.Tag(pkg, tag), err)
		}
		imageVersion = image.Version
	}

	version := ""
	if keepTag == "" {
		versionParts := strings.Split(imageVersion, "/")
		version = strings.ReplaceAll(versionParts[len(versionParts)-1], ":", "-")

		keepTag = (t.tagPrefix + version)
	}

	// Tag the image
	return version, keepTag, t.ApplyImageTag(ctx, reg, imageVersion, pkg, keepTag)
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

func missingVersionErr(err error) bool {
	// Artifact Registry reports missing versions as HTTP 400 when creating tags.
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 400 &&
		strings.EqualFold(strings.TrimSpace(apiErr.Message), "the referenced version does not exist")
}

func notFoundErr(err error) bool {
	apiErr := &apierror.APIError{}
	if errors.As(err, &apiErr) {
		if apiErr.HTTPCode() != -1 {
			// -1 is returned when the error is not an API error
			return apiErr.HTTPCode() == 404
		}
		return apiErr.GRPCStatus().Code() == codes.NotFound
	}
	return false
}

func alreadyExistsErr(err error) bool {
	apiErr := &apierror.APIError{}
	if errors.As(err, &apiErr) {
		if apiErr.HTTPCode() != -1 {
			// -1 is returned when the error is not an API error
			return apiErr.HTTPCode() == 409
		}
		return apiErr.GRPCStatus().Code() == codes.AlreadyExists
	}
	return false
}
