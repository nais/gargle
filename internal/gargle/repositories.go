package gargle

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
)

type Repository struct {
	Name string
	URL  string
}

func (r Repository) Image(pkg string) string {
	return r.Name + "/packages/" + strings.ReplaceAll(pkg, "/", "%2F")
}

func (r Repository) Tag(pkg, tag string) string {
	return r.Image(pkg) + "/tags/" + tag
}

type Repositories []Repository

func (r Repositories) URLs() []string {
	urls := make([]string, len(r))
	for i, repo := range r {
		urls[i] = repo.URL
	}
	return urls
}

var regMatchRegistry = regexp.MustCompile(`^projects\/([^\/]+)\/locations\/([^\/]+)\/repositories\/([^\/]+)$`)

func getRepositories(ctx context.Context, log *logrus.Logger, parent string, client *artifactregistry.Client) (Repositories, error) {
	repos := client.ListRepositories(ctx, &artifactregistrypb.ListRepositoriesRequest{
		Parent: parent, //"projects/" + projectID + "/locations/" + location,
	})

	var registries Repositories

	for {
		repo, err := repos.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}

		if repo.Format != artifactregistrypb.Repository_DOCKER || repo.Mode != artifactregistrypb.Repository_STANDARD_REPOSITORY || repo.Labels == nil {
			continue
		}

		if _, ok := repo.Labels["team"]; !ok {
			continue
		}

		parts := regMatchRegistry.FindStringSubmatch(repo.Name)
		if len(parts) == 0 {
			log.Warn("invalid registry:", repo.Name)
			continue
		}
		registries = append(registries, Repository{
			Name: repo.Name,
			URL:  fmt.Sprintf("%v-docker.pkg.dev/%v/%v", parts[2], parts[1], parts[3]),
		})
	}

	return registries, nil
}
