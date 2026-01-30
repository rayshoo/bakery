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

// LoadK8sServerConfig - 서버 측 K8s 설정 파일 읽기
func LoadK8sServerConfig(path string) (*K8sServerConfig, error) {
	if path == "" {
		return nil, nil // K8s 설정 없음 (ECS만 사용)
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
