package gargle

import (
	"fmt"
	"os"

	"github.com/ohler55/ojg/jp"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type ResourcePath = struct {
	Resource string   `yaml:"resource"`
	JSONPath JSONPath `yaml:"jsonpath"`
}

type GroupVersion struct {
	schema.GroupVersion
}

func (g *GroupVersion) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	gv, err := schema.ParseGroupVersion(s)
	if err != nil {
		return err
	}

	g.GroupVersion = gv
	return nil
}

type JSONPath struct {
	jp.Expr
}

func (j *JSONPath) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}

	expr, err := jp.ParseString(s)
	if err != nil {
		return err
	}

	j.Expr = expr
	return nil
}

type ImagesFrom map[GroupVersion][]ResourcePath

type Config struct {
	ProjectID  string     `yaml:"project-id"`
	Name       string     `yaml:"name"`
	Location   string     `yaml:"location"`
	Kubeconfig string     `yaml:"kubeconfig"`
	ImagesFrom ImagesFrom `yaml:"images-from"`
	LogLevel   string     `yaml:"log-level"`
}

func getConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}

	switch {
	case cfg.ProjectID == "":
		return Config{}, fmt.Errorf("project-id is required")
	case cfg.Name == "":
		return Config{}, fmt.Errorf("name is required")
	case len(cfg.ImagesFrom) == 0:
		return Config{}, fmt.Errorf("images-from is required")
	}
	return cfg, nil
}
