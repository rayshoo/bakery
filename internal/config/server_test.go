package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadK8sServerConfig(t *testing.T) {
	t.Run("empty path returns nil", func(t *testing.T) {
		cfg, err := LoadK8sServerConfig("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil, got %v", cfg)
		}
	})

	t.Run("non-existent file returns error", func(t *testing.T) {
		_, err := LoadK8sServerConfig("/no/such/file.yaml")
		if err == nil {
			t.Fatal("expected error for non-existent file")
		}
	})

	t.Run("valid yaml", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "k8s.yaml")
		data := []byte(`
k8s:
  serviceAccountName: builder
  imagePullSecrets:
    - name: regcred
  nodeSelector:
    node-type: build
  tolerations:
    - key: dedicated
      operator: Equal
      value: build
      effect: NoSchedule
`)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		cfg, err := LoadK8sServerConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if cfg.ServiceAccountName == nil || *cfg.ServiceAccountName != "builder" {
			t.Errorf("ServiceAccountName = %v, want %q", cfg.ServiceAccountName, "builder")
		}
		if len(cfg.ImagePullSecrets) != 1 || cfg.ImagePullSecrets[0].Name != "regcred" {
			t.Errorf("ImagePullSecrets = %v, want [{Name:regcred}]", cfg.ImagePullSecrets)
		}
		if cfg.NodeSelector["node-type"] != "build" {
			t.Errorf("NodeSelector = %v, want node-type=build", cfg.NodeSelector)
		}
		if len(cfg.Tolerations) != 1 || cfg.Tolerations[0].Key != "dedicated" {
			t.Errorf("Tolerations = %v, want [{Key:dedicated ...}]", cfg.Tolerations)
		}
	})

	t.Run("invalid yaml returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(path, []byte(`{{{invalid`), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		_, err := LoadK8sServerConfig(path)
		if err == nil {
			t.Fatal("expected error for invalid yaml")
		}
	})
}
