package config

import (
	"testing"
)

func boolP(v bool) *bool    { return &v }
func strP(v string) *string { return &v }

func TestUnmarshalYAML(t *testing.T) {
	t.Run("valid yaml", func(t *testing.T) {
		data := []byte(`
global:
  arch: amd64
  platform: ecs
bake:
  - arch: arm64
`)
		var cfg BuildConfig
		if err := UnmarshalYAML(data, &cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Global.Arch != "amd64" {
			t.Errorf("global.arch = %q, want %q", cfg.Global.Arch, "amd64")
		}
		if len(cfg.Bake) != 1 {
			t.Fatalf("len(bake) = %d, want 1", len(cfg.Bake))
		}
		if cfg.Bake[0].Arch != "arm64" {
			t.Errorf("bake[0].arch = %q, want %q", cfg.Bake[0].Arch, "arm64")
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		data := []byte(`{{{invalid`)
		var cfg BuildConfig
		if err := UnmarshalYAML(data, &cfg); err == nil {
			t.Fatal("expected error for invalid yaml")
		}
	})

	t.Run("empty yaml", func(t *testing.T) {
		data := []byte(``)
		var cfg BuildConfig
		if err := UnmarshalYAML(data, &cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestBuildEffectiveList(t *testing.T) {
	t.Run("nil config returns error", func(t *testing.T) {
		_, err := BuildEffectiveList(nil)
		if err == nil {
			t.Fatal("expected error for nil config")
		}
	})

	t.Run("arch not specified returns error", func(t *testing.T) {
		cfg := &BuildConfig{
			Bake: []BakeConfig{{}},
		}
		_, err := BuildEffectiveList(cfg)
		if err == nil {
			t.Fatal("expected error when arch not specified")
		}
	})

	t.Run("platform defaults to ecs", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64"},
			Bake:   []BakeConfig{{}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].Platform != "ecs" {
			t.Errorf("platform = %q, want %q", list[0].Platform, "ecs")
		}
	})

	t.Run("platform bake override", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64", Platform: "ecs"},
			Bake:   []BakeConfig{{Platform: "k8s"}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].Platform != "k8s" {
			t.Errorf("platform = %q, want %q", list[0].Platform, "k8s")
		}
	})

	t.Run("platform uses global", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64", Platform: "k8s"},
			Bake:   []BakeConfig{{}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].Platform != "k8s" {
			t.Errorf("platform = %q, want %q", list[0].Platform, "k8s")
		}
	})

	t.Run("arch bake override", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64"},
			Bake:   []BakeConfig{{Arch: "arm64"}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].Arch != "arm64" {
			t.Errorf("arch = %q, want %q", list[0].Arch, "arm64")
		}
	})

	t.Run("env map merge with bake priority", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Env:  map[string]string{"A": "global", "B": "global"},
			},
			Bake: []BakeConfig{{
				Env: map[string]string{"B": "bake", "C": "bake"},
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ef := list[0]
		if ef.Env["A"] != "global" {
			t.Errorf("Env[A] = %q, want %q", ef.Env["A"], "global")
		}
		if ef.Env["B"] != "bake" {
			t.Errorf("Env[B] = %q, want %q", ef.Env["B"], "bake")
		}
		if ef.Env["C"] != "bake" {
			t.Errorf("Env[C] = %q, want %q", ef.Env["C"], "bake")
		}
	})

	t.Run("build-args merge", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Kaniko: KanikoConfig{
					BuildArgs: map[string]string{"X": "global", "Y": "global"},
				},
			},
			Bake: []BakeConfig{{
				Kaniko: KanikoOverride{
					BuildArgs: map[string]string{"Y": "bake", "Z": "bake"},
				},
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ef := list[0]
		if ef.BuildArgs["X"] != "global" {
			t.Errorf("BuildArgs[X] = %q, want %q", ef.BuildArgs["X"], "global")
		}
		if ef.BuildArgs["Y"] != "bake" {
			t.Errorf("BuildArgs[Y] = %q, want %q", ef.BuildArgs["Y"], "bake")
		}
		if ef.BuildArgs["Z"] != "bake" {
			t.Errorf("BuildArgs[Z] = %q, want %q", ef.BuildArgs["Z"], "bake")
		}
	})

	t.Run("pre/post script override and fallback", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch:       "amd64",
				PreScript:  strP("global-pre"),
				PostScript: strP("global-post"),
			},
			Bake: []BakeConfig{{
				PreScript: strP("bake-pre"),
				// PostScript nil -> fallback to global
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ef := list[0]
		if ef.PreScript == nil || *ef.PreScript != "bake-pre" {
			t.Errorf("PreScript = %v, want %q", ef.PreScript, "bake-pre")
		}
		if ef.PostScript == nil || *ef.PostScript != "global-post" {
			t.Errorf("PostScript = %v, want %q", ef.PostScript, "global-post")
		}
	})

	t.Run("kaniko credentials all-or-nothing", func(t *testing.T) {
		globalCreds := []RegistryCredential{{Registry: "gcr.io", Username: "u1", Password: "p1"}}
		bakeCreds := []RegistryCredential{{Registry: "ecr", Username: "u2", Password: "p2"}}

		// bake has credentials -> use bake
		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64", KanikoCredentials: globalCreds},
			Bake:   []BakeConfig{{KanikoCredentials: bakeCreds}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(list[0].KanikoCredentials) != 1 || list[0].KanikoCredentials[0].Registry != "ecr" {
			t.Errorf("expected bake credentials, got %v", list[0].KanikoCredentials)
		}

		// bake has no credentials -> use global
		cfg2 := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64", KanikoCredentials: globalCreds},
			Bake:   []BakeConfig{{}},
		}
		list2, err := BuildEffectiveList(cfg2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(list2[0].KanikoCredentials) != 1 || list2[0].KanikoCredentials[0].Registry != "gcr.io" {
			t.Errorf("expected global credentials, got %v", list2[0].KanikoCredentials)
		}
	})

	t.Run("cache nil uses global entirely", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Kaniko: KanikoConfig{
					Cache: struct {
						Enable     *bool  `yaml:"enable,omitempty"`
						Repo       string `yaml:"repo,omitempty"`
						TTL        string `yaml:"ttl,omitempty"`
						CopyLayers *bool  `yaml:"copy-layers,omitempty"`
						RunLayers  *bool  `yaml:"run-layers,omitempty"`
						Compressed *bool  `yaml:"compressed,omitempty"`
					}{
						Enable: boolP(true),
						Repo:   "cache-repo",
						TTL:    "24h",
					},
				},
			},
			Bake: []BakeConfig{{
				Kaniko: KanikoOverride{
					Cache: nil, // nil -> use global entirely
				},
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ef := list[0]
		if ef.CacheEnable == nil || *ef.CacheEnable != true {
			t.Errorf("CacheEnable = %v, want true", ef.CacheEnable)
		}
		if ef.CacheRepo != "cache-repo" {
			t.Errorf("CacheRepo = %q, want %q", ef.CacheRepo, "cache-repo")
		}
		if ef.CacheTTL != "24h" {
			t.Errorf("CacheTTL = %q, want %q", ef.CacheTTL, "24h")
		}
	})

	t.Run("cache non-nil merges field by field", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Kaniko: KanikoConfig{
					Cache: struct {
						Enable     *bool  `yaml:"enable,omitempty"`
						Repo       string `yaml:"repo,omitempty"`
						TTL        string `yaml:"ttl,omitempty"`
						CopyLayers *bool  `yaml:"copy-layers,omitempty"`
						RunLayers  *bool  `yaml:"run-layers,omitempty"`
						Compressed *bool  `yaml:"compressed,omitempty"`
					}{
						Enable: boolP(true),
						Repo:   "global-repo",
						TTL:    "24h",
					},
				},
			},
			Bake: []BakeConfig{{
				Kaniko: KanikoOverride{
					Cache: &struct {
						Enable     *bool   `yaml:"enable"`
						Repo       *string `yaml:"repo"`
						TTL        *string `yaml:"ttl"`
						CopyLayers *bool   `yaml:"copy-layers"`
						RunLayers  *bool   `yaml:"run-layers"`
						Compressed *bool   `yaml:"compressed"`
					}{
						Repo: strP("bake-repo"),
						// Enable nil -> global, TTL nil -> global
					},
				},
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ef := list[0]
		if ef.CacheEnable == nil || *ef.CacheEnable != true {
			t.Errorf("CacheEnable = %v, want true (from global)", ef.CacheEnable)
		}
		if ef.CacheRepo != "bake-repo" {
			t.Errorf("CacheRepo = %q, want %q", ef.CacheRepo, "bake-repo")
		}
		if ef.CacheTTL != "24h" {
			t.Errorf("CacheTTL = %q, want %q (from global)", ef.CacheTTL, "24h")
		}
	})

	t.Run("destination nil uses empty string", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Kaniko: KanikoConfig{
					Destination: "global-dest",
				},
			},
			Bake: []BakeConfig{{
				Kaniko: KanikoOverride{
					Destination: nil,
				},
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].Destination != "" {
			t.Errorf("Destination = %q, want empty string", list[0].Destination)
		}
	})

	t.Run("cpu memory env fallback", func(t *testing.T) {
		t.Setenv("DEFAULT_BUILD_CPU", "2")
		t.Setenv("DEFAULT_BUILD_MEMORY", "4096")

		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64"},
			Bake:   []BakeConfig{{}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].CPU != "2" {
			t.Errorf("CPU = %q, want %q", list[0].CPU, "2")
		}
		if list[0].Memory != "4096" {
			t.Errorf("Memory = %q, want %q", list[0].Memory, "4096")
		}
	})

	t.Run("cpu memory bake overrides env", func(t *testing.T) {
		t.Setenv("DEFAULT_BUILD_CPU", "2")
		t.Setenv("DEFAULT_BUILD_MEMORY", "4096")

		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64"},
			Bake:   []BakeConfig{{CPU: "4", Memory: "8192"}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if list[0].CPU != "4" {
			t.Errorf("CPU = %q, want %q", list[0].CPU, "4")
		}
		if list[0].Memory != "8192" {
			t.Errorf("Memory = %q, want %q", list[0].Memory, "8192")
		}
	})

	t.Run("ignore-path all-or-nothing", func(t *testing.T) {
		// bake has ignore-path -> use bake
		cfg := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Kaniko: KanikoConfig{
					IgnorePath: []string{"/global"},
				},
			},
			Bake: []BakeConfig{{
				Kaniko: KanikoOverride{
					IgnorePath: []string{"/bake1", "/bake2"},
				},
			}},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(list[0].IgnorePath) != 2 || list[0].IgnorePath[0] != "/bake1" {
			t.Errorf("IgnorePath = %v, want [/bake1 /bake2]", list[0].IgnorePath)
		}

		// bake has no ignore-path -> use global
		cfg2 := &BuildConfig{
			Global: GlobalConfig{
				Arch: "amd64",
				Kaniko: KanikoConfig{
					IgnorePath: []string{"/global"},
				},
			},
			Bake: []BakeConfig{{
				Kaniko: KanikoOverride{},
			}},
		}
		list2, err := BuildEffectiveList(cfg2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(list2[0].IgnorePath) != 1 || list2[0].IgnorePath[0] != "/global" {
			t.Errorf("IgnorePath = %v, want [/global]", list2[0].IgnorePath)
		}
	})

	t.Run("multiple bake entries", func(t *testing.T) {
		cfg := &BuildConfig{
			Global: GlobalConfig{Arch: "amd64"},
			Bake: []BakeConfig{
				{Platform: "ecs"},
				{Platform: "k8s"},
				{Arch: "arm64"},
			},
		}
		list, err := BuildEffectiveList(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("len(list) = %d, want 3", len(list))
		}
		if list[0].Platform != "ecs" {
			t.Errorf("list[0].Platform = %q, want %q", list[0].Platform, "ecs")
		}
		if list[1].Platform != "k8s" {
			t.Errorf("list[1].Platform = %q, want %q", list[1].Platform, "k8s")
		}
		if list[2].Arch != "arm64" {
			t.Errorf("list[2].Arch = %q, want %q", list[2].Arch, "arm64")
		}
	})
}
