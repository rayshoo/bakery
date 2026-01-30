package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type GlobalConfig struct {
	Platform string            `yaml:"platform"`
	Arch     string            `yaml:"arch"`
	Env      map[string]string `yaml:"env"`
	CPU      string            `yaml:"cpu"`
	Memory   string            `yaml:"memory"`

	PreScript  *string `yaml:"pre-script"`
	PostScript *string `yaml:"post-script"`

	KanikoCredentials []RegistryCredential `yaml:"kaniko-credentials"`
	Kaniko            KanikoConfig         `yaml:"kaniko"`
}

type BakeConfig struct {
	Platform string            `yaml:"platform"`
	Arch     string            `yaml:"arch"`
	Env      map[string]string `yaml:"env"`
	CPU      string            `yaml:"cpu"`
	Memory   string            `yaml:"memory"`

	PreScript  *string `yaml:"pre-script"`
	PostScript *string `yaml:"post-script"`

	KanikoCredentials []RegistryCredential `yaml:"kaniko-credentials"`
	Kaniko            KanikoOverride       `yaml:"kaniko"`
}

type RegistryCredential struct {
	Registry string `yaml:"registry"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// KanikoConfig 는 Global 섹션의 Kaniko 설정을 담는 구조체입니다.
type KanikoConfig struct {
	ContextPath string            `yaml:"context-path"`
	Dockerfile  string            `yaml:"dockerfile"`
	BuildArgs   map[string]string `yaml:"build-args"`

	Cache struct {
		Enable     *bool  `yaml:"enable,omitempty"`
		Repo       string `yaml:"repo,omitempty"`
		TTL        string `yaml:"ttl,omitempty"`
		CopyLayers *bool  `yaml:"copy-layers,omitempty"`
		RunLayers  *bool  `yaml:"run-layers,omitempty"`
		Compressed *bool  `yaml:"compressed,omitempty"`
	} `yaml:"cache"`

	SnapshotMode   *string `yaml:"snapshot-mode,omitempty"`
	UseNewRun      *bool   `yaml:"use-new-run,omitempty"`
	Cleanup        *bool   `yaml:"cleanup,omitempty"`
	CustomPlatform *string `yaml:"custom-platform,omitempty"`
	Destination    string  `yaml:"destination"`

	NoPush     *bool    `yaml:"no-push,omitempty"`
	IgnorePath []string `yaml:"ignore-path,omitempty"`
	ExtraFlags string   `yaml:"extra-flags,omitempty"`
}

// KanikoOverride 는 Bake 섹션에서 Global 설정을 오버라이드하기 위한 구조체입니다.
type KanikoOverride struct {
	ContextPath *string           `yaml:"context-path"`
	Dockerfile  *string           `yaml:"dockerfile"`
	BuildArgs   map[string]string `yaml:"build-args"`

	Cache *struct {
		Enable     *bool   `yaml:"enable"`
		Repo       *string `yaml:"repo"`
		TTL        *string `yaml:"ttl"`
		CopyLayers *bool   `yaml:"copy-layers"`
		RunLayers  *bool   `yaml:"run-layers"`
		Compressed *bool   `yaml:"compressed"`
	} `yaml:"cache"`

	SnapshotMode   *string `yaml:"snapshot-mode"`
	UseNewRun      *bool   `yaml:"use-new-run"`
	Cleanup        *bool   `yaml:"cleanup"`
	CustomPlatform *string `yaml:"custom-platform"`
	Destination    *string `yaml:"destination"`

	NoPush     *bool    `yaml:"no-push"`
	IgnorePath []string `yaml:"ignore-path"`
	ExtraFlags *string  `yaml:"extra-flags"`
}

type LocalSecretRef struct {
	Name string `yaml:"name"`
}

type TolerationItem struct {
	Key      string `yaml:"key"`
	Operator string `yaml:"operator"`
	Value    string `yaml:"value"`
	Effect   string `yaml:"effect"`
}

type BuildConfig struct {
	Global GlobalConfig `yaml:"global"`
	Bake   []BakeConfig `yaml:"bake"`
}

// EffectiveConfig 는 Global 과 Bake 설정을 병합한 최종 실행 설정입니다.
type EffectiveConfig struct {
	Platform string
	Arch     string

	Env    map[string]string
	CPU    string
	Memory string

	PreScript  *string
	PostScript *string

	KanikoCredentials []RegistryCredential

	ContextPath string
	Dockerfile  string
	BuildArgs   map[string]string
	Destination string

	CacheEnable     *bool
	CacheRepo       string
	CacheTTL        string
	CacheCopyLayers *bool
	CacheRunLayers  *bool
	CacheCompressed *bool

	SnapshotMode   *string
	UseNewRun      *bool
	Cleanup        *bool
	CustomPlatform *string

	NoPush     *bool
	IgnorePath []string
	ExtraFlags string
}

func UnmarshalYAML(b []byte, out *BuildConfig) error {
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("invalid yaml: %w", err)
	}
	return nil
}

// BuildEffectiveList 는 BuildConfig 를 파싱하여 각 Bake 항목에 대한 EffectiveConfig 목록을 생성합니다.
func BuildEffectiveList(cfg *BuildConfig) ([]EffectiveConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}

	var list []EffectiveConfig
	global := cfg.Global

	defaultCPU := os.Getenv("DEFAULT_BUILD_CPU")
	defaultMemory := os.Getenv("DEFAULT_BUILD_MEMORY")

	for _, b := range cfg.Bake {

		ef := EffectiveConfig{}

		if b.Platform != "" {
			ef.Platform = b.Platform
		} else if global.Platform != "" {
			ef.Platform = global.Platform
		} else {
			ef.Platform = "ecs"
		}

		if b.Arch != "" {
			ef.Arch = b.Arch
		} else if global.Arch != "" {
			ef.Arch = global.Arch
		} else {
			return nil, fmt.Errorf("arch not specified in either global or bake section")
		}

		ef.CPU = coalesceStr(b.CPU, global.CPU, defaultCPU)
		ef.Memory = coalesceStr(b.Memory, global.Memory, defaultMemory)

		ef.Env = map[string]string{}
		for k, v := range global.Env {
			ef.Env[k] = v
		}
		for k, v := range b.Env {
			ef.Env[k] = v
		}

		if b.PreScript != nil {
			ef.PreScript = b.PreScript
		} else {
			ef.PreScript = global.PreScript
		}

		if b.PostScript != nil {
			ef.PostScript = b.PostScript
		} else {
			ef.PostScript = global.PostScript
		}

		if len(b.KanikoCredentials) > 0 {
			ef.KanikoCredentials = b.KanikoCredentials
		} else {
			ef.KanikoCredentials = global.KanikoCredentials
		}

		if b.Kaniko.ContextPath != nil {
			ef.ContextPath = *b.Kaniko.ContextPath
		} else {
			ef.ContextPath = global.Kaniko.ContextPath
		}

		if b.Kaniko.Dockerfile != nil {
			ef.Dockerfile = *b.Kaniko.Dockerfile
		} else {
			ef.Dockerfile = global.Kaniko.Dockerfile
		}

		ef.BuildArgs = map[string]string{}
		for k, v := range global.Kaniko.BuildArgs {
			ef.BuildArgs[k] = v
		}
		for k, v := range b.Kaniko.BuildArgs {
			ef.BuildArgs[k] = v
		}

		if b.Kaniko.Cache != nil {
			ef.CacheEnable = boolPtr(b.Kaniko.Cache.Enable, global.Kaniko.Cache.Enable)

			if b.Kaniko.Cache.Repo != nil {
				ef.CacheRepo = *b.Kaniko.Cache.Repo
			} else {
				ef.CacheRepo = global.Kaniko.Cache.Repo
			}

			if b.Kaniko.Cache.TTL != nil {
				ef.CacheTTL = *b.Kaniko.Cache.TTL
			} else {
				ef.CacheTTL = global.Kaniko.Cache.TTL
			}

			ef.CacheCopyLayers = boolPtr(b.Kaniko.Cache.CopyLayers, global.Kaniko.Cache.CopyLayers)
			ef.CacheRunLayers = boolPtr(b.Kaniko.Cache.RunLayers, global.Kaniko.Cache.RunLayers)
			ef.CacheCompressed = boolPtr(b.Kaniko.Cache.Compressed, global.Kaniko.Cache.Compressed)
		} else {
			ef.CacheEnable = global.Kaniko.Cache.Enable
			ef.CacheRepo = global.Kaniko.Cache.Repo
			ef.CacheTTL = global.Kaniko.Cache.TTL
			ef.CacheCopyLayers = global.Kaniko.Cache.CopyLayers
			ef.CacheRunLayers = global.Kaniko.Cache.RunLayers
			ef.CacheCompressed = global.Kaniko.Cache.Compressed
		}

		ef.SnapshotMode = strPtr(b.Kaniko.SnapshotMode, global.Kaniko.SnapshotMode)
		ef.UseNewRun = boolPtr(b.Kaniko.UseNewRun, global.Kaniko.UseNewRun)
		ef.Cleanup = boolPtr(b.Kaniko.Cleanup, global.Kaniko.Cleanup)
		ef.CustomPlatform = strPtr(b.Kaniko.CustomPlatform, global.Kaniko.CustomPlatform)

		ef.NoPush = boolPtr(b.Kaniko.NoPush, global.Kaniko.NoPush)

		if len(b.Kaniko.IgnorePath) > 0 {
			ef.IgnorePath = b.Kaniko.IgnorePath
		} else {
			ef.IgnorePath = global.Kaniko.IgnorePath
		}

		if b.Kaniko.ExtraFlags != nil {
			ef.ExtraFlags = *b.Kaniko.ExtraFlags
		} else {
			ef.ExtraFlags = global.Kaniko.ExtraFlags
		}

		if b.Kaniko.Destination != nil {
			ef.Destination = *b.Kaniko.Destination
		} else {
			ef.Destination = ""
		}

		list = append(list, ef)
	}

	return list, nil
}

func coalesceStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func boolPtr(override *bool, global *bool) *bool {
	if override != nil {
		return override
	}
	return global
}

func strPtr(override *string, global *string) *string {
	if override != nil {
		return override
	}
	return global
}
