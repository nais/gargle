package gargle

import (
	"context"
	"flag"
	"fmt"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func Main(ctx context.Context) error {
	config := "./config.yaml"
	flag.StringVar(&config, "config", config, "Config file")

	flag.Parse()

	cfg, err := getConfig(config)
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("Starting gargle")

	artClient, err := artifactregistry.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create artifact registry client: %w", err)
	}

	parent := "projects/" + cfg.ProjectID + "/locations/" + cfg.Location
	start := time.Now()
	registries, err := getRepositories(ctx, log, parent, artClient)
	if err != nil {
		return fmt.Errorf("failed to get repositories: %w", err)
	}

	log.Debugln("Got repositories in", time.Since(start))

	k8sConfig, err := clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build k8s config: %w", err)
	}
	client, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	start = time.Now()
	ig := NewImageGatherer(cfg.ImagesFrom)
	if err := ig.Gather(ctx, client, registries.URLs()); err != nil {
		return fmt.Errorf("failed to gather images: %w", err)
	}
	log.Debugln("Gathered images in", time.Since(start))

	tagger := NewTagger(ctx, log, artClient, cfg.Name, ig.imageList)
	defer tagger.Close()

	start = time.Now()
	if err := tagger.Run(ctx, registries); err != nil {
		return fmt.Errorf("failed to tag images: %w", err)
	}
	log.Debugln("Tagged images in", time.Since(start))

	return nil
}

func newLogger(logLevel string) *logrus.Logger {
	log := logrus.StandardLogger()
	log.SetFormatter(&logrus.JSONFormatter{})

	l, err := logrus.ParseLevel(logLevel)
	if err != nil {
		log.Fatal(err)
	}
	log.SetLevel(l)
	return log
}
