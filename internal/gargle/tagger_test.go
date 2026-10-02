package gargle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
