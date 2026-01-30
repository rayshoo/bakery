package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type K8sServerConfig struct {
	ImagePullSecrets   []LocalSecretRef  `yaml:"imagePullSecrets"`
	ServiceAccountName *string           `yaml:"serviceAccountName"`
	NodeSelector       map[string]string `yaml:"nodeSelector"`
	Tolerations        []TolerationItem  `yaml:"tolerations"`
}

// LoadK8sServerConfig loads the server-side K8s configuration file.
func LoadK8sServerConfig(path string) (*K8sServerConfig, error) {
	if path == "" {
		return nil, nil // No K8s config (ECS-only mode)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read k8s config file: %w", err)
	}

	var cfg struct {
		K8s K8sServerConfig `yaml:"k8s"`
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse k8s config: %w", err)
	}

	return &cfg.K8s, nil
}
