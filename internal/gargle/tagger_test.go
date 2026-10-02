package gargle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/option"
)

const testDigest = "sha256:76352153926ed83fcf978664cc1e4ebc8cfefafda837a93015661c64314381f6"

var testRepository = Repository{
	Name: "projects/project/locations/europe-north1/repositories/team",
	URL:  "europe-north1-docker.pkg.dev/project/team",
}

type testHandlerTransport struct {
	handler http.HandlerFunc
}

func (transport testHandlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, r)
	return response.Result(), nil
}

func newTestArtifactClient(t *testing.T, handler http.HandlerFunc) *artifactregistry.Client {
	t.Helper()
	client, err := artifactregistry.NewRESTClient(
		context.Background(),
		option.WithEndpoint("http://artifactregistry.test"),
		option.WithoutAuthentication(),
		option.WithHTTPClient(&http.Client{Transport: testHandlerTransport{handler: handler}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return client
}

func TestTagRegistryReferences(t *testing.T) {
	for _, reference := range []string{":release", "@" + testDigest, ":release@" + testDigest} {
		t.Run(reference, func(t *testing.T) {
			var created []*artifactregistrypb.Tag
			var lookups []string
			client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					lookups = append(lookups, tag)
					switch tag {
					case "release":
						if reference != ":release" {
							t.Error("pinned image was resolved through its mutable tag")
						}
						if err := json.NewEncoder(w).Encode(&artifactregistrypb.Tag{
							Version: testRepository.Image("app") + "/versions/" + testDigest,
						}); err != nil {
							t.Error(err)
						}
					case strings.ReplaceAll(testDigest, ":", "-") + ".att":
						if err := json.NewEncoder(w).Encode(&artifactregistrypb.Tag{
							Version: testRepository.Image("app") + "/versions/sha256:attestation",
						}); err != nil {
							t.Error(err)
						}
					default:
						w.WriteHeader(http.StatusNotFound)
						_, err := w.Write([]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`))
						if err != nil {
							t.Error(err)
						}
					}
				case http.MethodPost:
					tag := &artifactregistrypb.Tag{}
					if err := json.NewDecoder(r.Body).Decode(tag); err != nil {
						t.Error(err)
					}
					created = append(created, tag)
					if err := json.NewEncoder(w).Encode(tag); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			images := NewImageGatherer(nil).imageList
			images.AddImage(testRepository.URL + "/app" + reference)
			tagger := NewTagger(context.Background(), logrus.New(), client, "test", images)
			if err := tagger.TagRegistry(context.Background(), testRepository); err != nil {
				t.Fatal(err)
			}
			if len(created) != 2 {
				t.Fatalf("expected base and attestation tags despite missing signature, got %v", created)
			}
			wantTag := testRepository.Tag("app", "keep-nais-test-"+strings.ReplaceAll(testDigest, ":", "-"))
			if created[0].Name != wantTag || created[0].Version != testRepository.Image("app")+"/versions/"+testDigest {
				t.Fatalf("incorrect pinned tag: %v", created[0])
			}
			if created[1].Name != wantTag+".att" {
				t.Fatalf("incorrect attestation tag: %v", created[1])
			}
			wantLookups := 2
			if reference == ":release" {
				wantLookups++
			}
			if len(lookups) != wantLookups {
				t.Fatalf("unexpected tag lookups: %v", lookups)
			}
		})
	}
}

func TestCleanRepositoryReferences(t *testing.T) {
	for _, reference := range []string{":release", "@" + testDigest, ":release@" + testDigest} {
		t.Run(reference, func(t *testing.T) {
			var deleted []string
			client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					protectedTags := []string{"keep-nais-test-protected"}
					staleTags := []string{"keep-nais-test-stale"}
					if reference == ":release" {
						protectedTags = append(protectedTags, "release")
					} else {
						// The accompanying tag moved to another digest.
						staleTags = append(staleTags, "release")
					}
					response := map[string]any{"dockerImages": []*artifactregistrypb.DockerImage{
						{
							Name: testRepository.Name + "/dockerImages/app@" + testDigest,
							Uri:  testRepository.URL + "/app@" + testDigest,
							Tags: protectedTags,
						},
						{
							Name: testRepository.Name + "/dockerImages/app@sha256:stale",
							Uri:  testRepository.URL + "/app@sha256:stale",
							Tags: staleTags,
						},
					}}
					if err := json.NewEncoder(w).Encode(response); err != nil {
						t.Error(err)
					}
				case http.MethodDelete:
					deleted = append(deleted, r.URL.Path)
					if _, err := w.Write([]byte(`{}`)); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			images := NewImageGatherer(nil).imageList
			images.AddImage(testRepository.URL + "/app" + reference)
			tagger := NewTagger(context.Background(), logrus.New(), client, "test", images)
			if err := tagger.cleanRepository(context.Background(), testRepository.Name); err != nil {
				t.Fatal(err)
			}
			if len(deleted) != 3 {
				t.Fatalf("expected stale base, signature, and attestation tags to be removed, got %v", deleted)
			}
			for _, path := range deleted {
				if !strings.Contains(path, "/tags/keep-nais-test-stale") {
					t.Errorf("protected image was untagged: %s", path)
				}
			}
		})
	}
}

func TestKeepImagePropagatesErrors(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		method    string
		suffix    string
	}{
		{"tag lookup", "release", http.MethodGet, "/release"},
		{"digest tagging", testDigest, http.MethodPost, "/tags"},
		{"signature lookup", testDigest, http.MethodGet, ".sig"},
		{"attestation lookup", testDigest, http.MethodGet, ".att"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == tt.method && strings.HasSuffix(r.URL.Path, tt.suffix) {
					w.WriteHeader(http.StatusForbidden)
					if _, err := w.Write([]byte(`{"error":{"code":403,"message":"permission denied","status":"PERMISSION_DENIED"}}`)); err != nil {
						t.Error(err)
					}
				} else if r.Method == http.MethodPost {
					if _, err := w.Write([]byte(`{}`)); err != nil {
						t.Error(err)
					}
				} else {
					w.WriteHeader(http.StatusNotFound)
					if _, err := w.Write([]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`)); err != nil {
						t.Error(err)
					}
				}
			})
			tagger := NewTagger(context.Background(), logrus.New(), client, "test", NewImageGatherer(nil).imageList)
			err := tagger.KeepImage(context.Background(), testRepository, testRepository.URL+"/app", tt.reference)
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("expected permission error, got %v", err)
			}
		})
	}
}

func TestKeepImageExistingDigestTag(t *testing.T) {
	client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			if _, err := w.Write([]byte(`{"error":{"code":409,"message":"already exists","status":"ALREADY_EXISTS"}}`)); err != nil {
				t.Error(err)
			}
		} else {
			w.WriteHeader(http.StatusNotFound)
			if _, err := w.Write([]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`)); err != nil {
				t.Error(err)
			}
		}
	})
	tagger := NewTagger(context.Background(), logrus.New(), client, "test", NewImageGatherer(nil).imageList)
	if err := tagger.KeepImage(context.Background(), testRepository, testRepository.URL+"/app", testDigest); err != nil {
		t.Fatalf("existing keep-tag and missing auxiliary images should be harmless: %v", err)
	}
}

func writeTestAPIError(t *testing.T, w http.ResponseWriter, status int, message string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": status, "message": message},
	}); err != nil {
		t.Error(err)
	}
}

func TestTagRegistryContinuesAfterErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		failure bool
	}{
		{"missing version", http.StatusBadRequest, "the referenced version does not exist", false},
		{"not found", http.StatusNotFound, "not found", false},
		{"other bad request", http.StatusBadRequest, "invalid tag", true},
		{"permission denied", http.StatusForbidden, "permission denied", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var versions []string
			client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					writeTestAPIError(t, w, http.StatusNotFound, "optional image not found")
					return
				}
				tag := &artifactregistrypb.Tag{}
				if err := json.NewDecoder(r.Body).Decode(tag); err != nil {
					t.Fatal(err)
				}
				versions = append(versions, tag.Version)
				if strings.Contains(tag.Version, "sha256:sha256:") {
					writeTestAPIError(t, w, tt.status, tt.message)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(tag); err != nil {
					t.Error(err)
				}
			})
			badDigest := "sha256:" + testDigest
			images := &imageList{list: map[string][]string{
				testRepository.URL + "/app": {badDigest, testDigest, badDigest, testDigest},
			}}
			var logs bytes.Buffer
			log := logrus.New()
			log.SetOutput(&logs)
			tagger := NewTagger(context.Background(), log, client, "test", images)
			err := tagger.TagRegistry(context.Background(), testRepository)
			if tt.failure {
				if err == nil || strings.Count(err.Error(), tt.message) != 2 {
					t.Fatalf("expected both errors, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("missing images must not fail the run: %v", err)
				}
				if strings.Count(logs.String(), "Referenced image not found; skipping") != 2 {
					t.Fatalf("expected both missing images to be logged: %s", logs.String())
				}
			}
			if len(versions) != 4 || versions[1] != testRepository.Image("app")+"/versions/"+testDigest ||
				versions[3] != testRepository.Image("app")+"/versions/"+testDigest {
				t.Fatalf("valid references were not protected after failures: %v", versions)
			}
		})
	}
}

func TestRunContinuesAcrossRepositories(t *testing.T) {
	var lock sync.Mutex
	tagged := make(map[string]int)
	client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		_, resource, _ := strings.Cut(r.URL.Path, "/repositories/")
		repository, _, _ := strings.Cut(resource, "/")
		switch {
		case strings.HasSuffix(r.URL.Path, "/dockerImages"):
			if repository == "team-0" {
				writeTestAPIError(t, w, http.StatusForbidden, "cleanup denied")
				return
			}
		case r.Method == http.MethodPost:
			tagged[repository]++
			if repository == "team-1" {
				writeTestAPIError(t, w, http.StatusBadRequest, "tagging denied")
				return
			}
		default:
			writeTestAPIError(t, w, http.StatusNotFound, "optional image not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Error(err)
		}
	})
	images := NewImageGatherer(nil).imageList
	var repositories Repositories
	for index := range 6 {
		repository := Repository{
			Name: fmt.Sprintf("%s-%d", testRepository.Name, index),
			URL:  fmt.Sprintf("%s-%d", testRepository.URL, index),
		}
		repositories = append(repositories, repository)
		images.AddImage(repository.URL + "/app@" + testDigest)
	}
	tagger := NewTagger(context.Background(), logrus.New(), client, "test", images)
	err := tagger.Run(context.Background(), repositories)
	if err == nil || !strings.Contains(err.Error(), "cleanup denied") || !strings.Contains(err.Error(), "tagging denied") {
		t.Fatalf("expected cleanup and tagging errors, got %v", err)
	}
	for index := range 6 {
		if tagged[fmt.Sprintf("team-%d", index)] != 1 {
			t.Errorf("repository %d was not tagged: %v", index, tagged)
		}
	}
}

func TestCleanRepositoryContinuesAfterErrors(t *testing.T) {
	var deleted []string
	client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			var images []*artifactregistrypb.DockerImage
			for _, digest := range []string{"first", "second"} {
				images = append(images, &artifactregistrypb.DockerImage{
					Name: testRepository.Name + "/dockerImages/app@sha256:" + digest,
					Uri:  testRepository.URL + "/app@sha256:" + digest,
					Tags: []string{"keep-nais-test-" + digest},
				})
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"dockerImages": images}); err != nil {
				t.Error(err)
			}
			return
		}
		deleted = append(deleted, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/keep-nais-test-first") {
			writeTestAPIError(t, w, http.StatusForbidden, "delete denied")
			return
		}
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Error(err)
		}
	})
	tagger := NewTagger(context.Background(), logrus.New(), client, "test", NewImageGatherer(nil).imageList)
	err := tagger.cleanRepository(context.Background(), testRepository.Name)
	if err == nil || !strings.Contains(err.Error(), "delete denied") {
		t.Fatalf("expected deletion error, got %v", err)
	}
	if len(deleted) != 6 {
		t.Fatalf("expected all base and auxiliary deletions to be attempted, got %v", deleted)
	}
}

func TestKeepImageCollectsAuxiliaryErrors(t *testing.T) {
	var lookups []string
	client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			lookups = append(lookups, r.URL.Path)
			writeTestAPIError(t, w, http.StatusForbidden, "lookup denied")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Error(err)
		}
	})
	tagger := NewTagger(context.Background(), logrus.New(), client, "test", NewImageGatherer(nil).imageList)
	err := tagger.KeepImage(context.Background(), testRepository, testRepository.URL+"/app", testDigest)
	if err == nil || !strings.Contains(err.Error(), "sig image") || !strings.Contains(err.Error(), "att image") {
		t.Fatalf("expected both auxiliary errors, got %v", err)
	}
	if len(lookups) != 2 {
		t.Fatalf("expected both auxiliary lookups, got %v", lookups)
	}
}

func TestRunRespectsCancellation(t *testing.T) {
	client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request after cancellation: %s %s", r.Method, r.URL)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tagger := NewTagger(ctx, logrus.New(), client, "test", NewImageGatherer(nil).imageList)
	if err := tagger.Run(ctx, Repositories{testRepository}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestKeepImageMissingTag(t *testing.T) {
	client := newTestArtifactClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/tags/release") {
			t.Errorf("unexpected request after missing base tag: %s %s", r.Method, r.URL)
		}
		writeTestAPIError(t, w, http.StatusNotFound, "not found")
	})
	tagger := NewTagger(context.Background(), logrus.New(), client, "test", NewImageGatherer(nil).imageList)
	if err := tagger.KeepImage(context.Background(), testRepository, testRepository.URL+"/app", "release"); err != nil {
		t.Fatalf("missing base tag must be skipped: %v", err)
	}
}
