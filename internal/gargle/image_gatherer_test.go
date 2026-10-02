package gargle

import (
	"slices"
	"testing"
)

func TestImageListReferences(t *testing.T) {
	const name = "europe-north1-docker.pkg.dev/project/team/app"
	const digest = "sha256:76352153926ed83fcf978664cc1e4ebc8cfefafda837a93015661c64314381f6"
	tests := []struct {
		name      string
		image     string
		imageName string
		reference string
	}{
		{"tag", name + ":release", name, "release"},
		{"digest", name + "@" + digest, name, digest},
		{"tag and digest", name + ":release@" + digest, name, digest},
		{"registry port", "registry:5000/team/app:release", "registry:5000/team/app", "release"},
		{"registry port and digest", "registry:5000/team/app:release@" + digest, "registry:5000/team/app", digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			images := NewImageGatherer(nil).imageList
			images.AddImage(tt.image)
			images.AddImage(tt.image)
			if !images.HasImage(tt.imageName, tt.reference) {
				t.Fatalf("missing reference %q for %q: %v", tt.reference, tt.imageName, images.list)
			}
			if len(images.list) != 1 || len(images.list[tt.imageName]) != 1 {
				t.Fatalf("references were not deduplicated: %v", images.list)
			}
			if tt.reference == digest && images.HasImage(tt.imageName, "release") {
				t.Fatal("a pinned reference must not track its mutable tag")
			}
		})
	}
}

func TestImageListRepositoryBoundary(t *testing.T) {
	images := NewImageGatherer(nil).imageList
	images.AddImage("registry/project/team/app:release")
	images.AddImage("registry/project/team-other/app:release")
	got := images.ForPrefix("registry/project/team")
	if len(got) != 1 || !slices.Equal(got["registry/project/team/app"], []string{"release"}) {
		t.Fatalf("unexpected images for repository: %v", got)
	}
}
