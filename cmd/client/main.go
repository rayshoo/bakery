package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/compose-spec/compose-go/v2/interpolation"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gopkg.in/yaml.v3"
)

func loadEnv() { _ = godotenv.Load(".env") }

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tarGzDir(src string, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && filepath.Base(path) == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err = tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			var f *os.File
			f, err = os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func newS3Client(ctx context.Context) (*minio.Client, string, error) {
	endpoint := getenv("S3_ENDPOINT", "")
	region := getenv("S3_REGION", "us-east-1")
	bucket := getenv("S3_BUCKET", "")
	useSSL := getenv("S3_SSL", "false") == "true"

	if endpoint == "" || bucket == "" {
		return nil, "", fmt.Errorf("S3_ENDPOINT, S3_BUCKET env required")
	}

	accessKey := getenv("S3_ACCESS_KEY", "")
	secretKey := getenv("S3_SECRET_KEY", "")
	sessionToken := getenv("S3_SESSION_TOKEN", "")

	if accessKey != "" && secretKey != "" {
		cli, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(accessKey, secretKey, sessionToken),
			Region: region,
			Secure: useSSL,
		})
		if err != nil {
			return nil, "", err
		}
		return cli, bucket, nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	v, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, "", err
	}

	cli, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(v.AccessKeyID, v.SecretAccessKey, v.SessionToken),
		Region: region,
		Secure: useSSL,
	})
	if err != nil {
		return nil, "", err
	}
	return cli, bucket, nil
}

func uploadToS3(ctx context.Context, cli *minio.Client, bucket, object, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}

	_, err = cli.PutObject(ctx, bucket, object, f, st.Size(), minio.PutObjectOptions{
		ContentType: "application/gzip",
		PartSize:    5 << 20,
		NumThreads:  1,
	})
	return err
}

type ComposeFile struct {
	Services map[string]ComposeService `yaml:"services"`
}

type ComposeService struct {
	Build ComposeBuild `yaml:"build"`
	Image string       `yaml:"image"`
}

type ComposeBuild struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
	XBake      *XBake            `yaml:"x-bake"`
}

type XBake struct {
	Platforms []string `yaml:"platforms"`
}

type GlobalConfig struct {
	Platform          string                 `yaml:"platform"`
	Arch              string                 `yaml:"arch"`
	Env               map[string]string      `yaml:"env"`
	CPU               string                 `yaml:"cpu"`
	Memory            string                 `yaml:"memory"`
	PreScript         *string                `yaml:"pre-script"`
	PostScript        *string                `yaml:"post-script"`
	KanikoCredentials []RegistryCredential   `yaml:"kaniko-credentials"`
	Kaniko            map[string]interface{} `yaml:"kaniko"`
}

type BakeConfig struct {
	Platform          string                 `yaml:"platform"`
	Arch              string                 `yaml:"arch"`
	Env               map[string]string      `yaml:"env"`
	CPU               string                 `yaml:"cpu"`
	Memory            string                 `yaml:"memory"`
	PreScript         *string                `yaml:"pre-script"`
	PostScript        *string                `yaml:"post-script"`
	KanikoCredentials []RegistryCredential   `yaml:"kaniko-credentials"`
	Kaniko            map[string]interface{} `yaml:"kaniko"`
}

type RegistryCredential struct {
	Registry string `yaml:"registry"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type BuildConfig struct {
	Global GlobalConfig `yaml:"global"`
	Bake   []BakeConfig `yaml:"bake"`
}

type ServiceBuildConfig struct {
	ServiceName string
	Config      BuildConfig
}

// interpolateCompose 는 compose 환경 변수 보간을 적용합니다.
func interpolateCompose(composeBytes []byte) ([]byte, error) {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(composeBytes, &raw); err != nil {
		return nil, fmt.Errorf("parse compose file: %w", err)
	}

	lookup := func(key string) (string, bool) {
		return os.LookupEnv(key)
	}

	expanded, err := interpolation.Interpolate(raw, interpolation.Options{
		LookupValue: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("interpolate compose file: %w", err)
	}

	out, err := yaml.Marshal(expanded)
	if err != nil {
		return nil, fmt.Errorf("marshal compose file: %w", err)
	}

	return out, nil
}

// mergeComposeToConfig 는 docker-compose.yaml과 base config를 병합하여 서비스별 빌드 설정을 생성합니다.
func mergeComposeToConfig(baseConfig *BuildConfig, composePath string, services []string) ([]ServiceBuildConfig, error) {
	composeBytes, err := os.ReadFile(composePath)
	if err != nil {
		return nil, fmt.Errorf("read compose file: %w", err)
	}

	composeBytes, err = interpolateCompose(composeBytes)
	if err != nil {
		return nil, err
	}

	var compose ComposeFile
	if err := yaml.Unmarshal(composeBytes, &compose); err != nil {
		return nil, fmt.Errorf("parse compose file: %w", err)
	}

	var orderedServices []string
	if len(services) == 0 {
		for name := range compose.Services {
			orderedServices = append(orderedServices, name)
		}
	} else {
		orderedServices = services
	}

	selectedServices := make(map[string]ComposeService)
	for _, svc := range orderedServices {
		if s, ok := compose.Services[svc]; ok {
			selectedServices[svc] = s
		} else {
			return nil, fmt.Errorf("service %q not found in compose file", svc)
		}
	}

	if baseConfig == nil {
		baseConfig = &BuildConfig{}
	}

	var serviceBuildConfigs []ServiceBuildConfig

	for _, svcName := range orderedServices {
		svc := selectedServices[svcName]

		platforms := []string{}
		if svc.Build.XBake != nil && len(svc.Build.XBake.Platforms) > 0 {
			platforms = svc.Build.XBake.Platforms
		} else {
			arch := runtime.GOARCH
			platforms = append(platforms, "linux/"+arch)
		}

		serviceConfig := BuildConfig{
			Global: GlobalConfig{
				Platform:          baseConfig.Global.Platform,
				CPU:               baseConfig.Global.CPU,
				Memory:            baseConfig.Global.Memory,
				PreScript:         baseConfig.Global.PreScript,
				PostScript:        baseConfig.Global.PostScript,
				KanikoCredentials: baseConfig.Global.KanikoCredentials,
			},
			Bake: []BakeConfig{},
		}

		if len(baseConfig.Global.Env) > 0 {
			serviceConfig.Global.Env = make(map[string]string)
			for k, v := range baseConfig.Global.Env {
				serviceConfig.Global.Env[k] = v
			}
		}

		serviceConfig.Global.Kaniko = make(map[string]interface{})
		for k, v := range baseConfig.Global.Kaniko {
			serviceConfig.Global.Kaniko[k] = v
		}

		contextPath := svc.Build.Context
		if contextPath != "" {
			serviceConfig.Global.Kaniko["context-path"] = contextPath
		}

		dockerfile := svc.Build.Dockerfile
		if dockerfile != "" {
			serviceConfig.Global.Kaniko["dockerfile"] = dockerfile
		} else if _, exists := serviceConfig.Global.Kaniko["dockerfile"]; !exists {
			serviceConfig.Global.Kaniko["dockerfile"] = "Dockerfile"
		}

		finalBuildArgs := make(map[string]string)
		if globalArgs, ok := baseConfig.Global.Kaniko["build-args"].(map[string]interface{}); ok {
			for k, v := range globalArgs {
				if s, ok := v.(string); ok {
					finalBuildArgs[k] = s
				}
			}
		}
		for k, v := range svc.Build.Args {
			finalBuildArgs[k] = v
		}
		if len(finalBuildArgs) > 0 {
			serviceConfig.Global.Kaniko["build-args"] = finalBuildArgs
		}

		destination := svc.Image
		if destination != "" {
			serviceConfig.Global.Kaniko["destination"] = destination
		}

		for _, platform := range platforms {
			arch := strings.TrimPrefix(platform, "linux/")
			bake := BakeConfig{
				Arch: arch,
			}
			serviceConfig.Bake = append(serviceConfig.Bake, bake)
		}

		serviceBuildConfigs = append(serviceBuildConfigs, ServiceBuildConfig{
			ServiceName: svcName,
			Config:      serviceConfig,
		})

		log.Printf("Created BuildConfig for service: %s (architectures: %v)", svcName, platforms)
	}

	return serviceBuildConfigs, nil
}

type buildResponse struct {
	BuildID string `json:"buildID"`
	Status  string `json:"status"`
}

type logEntry struct {
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type buildResult struct {
	ServiceName string
	Error       error
}

var version = "dev"

func main() {
	loadEnv()

	var configPath = flag.String("config", "", "path to build config yaml file (optional)")
	var composePath = flag.String("compose", "", "path to docker-compose.yaml file (optional)")
	var servicesFlag = flag.String("services", "", "comma-separated list of services to build (empty = all)")
	var asyncMode = flag.Bool("async", false, "build services asynchronously")
	var repoPath = flag.String("repo", ".", "path to repository root")
	var showVersion = flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *configPath == "" && *composePath == "" {
		*configPath = "config.yaml"
	}

	ctx := context.Background()

	var baseConfig *BuildConfig
	if *configPath != "" {
		yamlBytes, err := os.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("read config: %v", err)
		}
		baseConfig = &BuildConfig{}
		if err := yaml.Unmarshal(yamlBytes, baseConfig); err != nil {
			log.Fatalf("parse config: %v", err)
		}
	}

	var serviceBuildConfigs []ServiceBuildConfig
	if *composePath != "" {
		services := []string{}
		if *servicesFlag != "" {
			services = strings.Split(*servicesFlag, ",")
			for i := range services {
				services[i] = strings.TrimSpace(services[i])
			}
		}

		var err error
		serviceBuildConfigs, err = mergeComposeToConfig(baseConfig, *composePath, services)
		if err != nil {
			log.Fatalf("merge compose: %v", err)
		}
	} else if baseConfig != nil && len(baseConfig.Bake) > 0 {
		serviceBuildConfigs = []ServiceBuildConfig{
			{
				ServiceName: "",
				Config:      *baseConfig,
			},
		}
	}

	if len(serviceBuildConfigs) == 0 {
		log.Fatal("No build configurations found")
	}

	s3Cli, bucket, err := newS3Client(ctx)
	if err != nil {
		log.Fatalf("newS3Client: %v", err)
	}

	tmpBase := getenv("TMPDIR", "/builds/tmp")
	_ = os.MkdirAll(tmpBase, 0o755)

	tmp := filepath.Join(tmpBase, fmt.Sprintf("repo-%d-%s.tar.gz", time.Now().Unix(), randHex(4)))
	f, err := os.Create(tmp)
	if err != nil {
		log.Fatalf("create temp: %v", err)
	}
	if err = tarGzDir(*repoPath, f); err != nil {
		log.Fatalf("tarGzDir: %v", err)
	}
	f.Close()
	defer os.Remove(tmp)

	object := fmt.Sprintf("repos/%d-%s/repo.tar.gz", time.Now().Unix(), randHex(4))
	log.Printf("Uploading to s3: %s/%s", bucket, object)
	if err = uploadToS3(ctx, s3Cli, bucket, object, tmp); err != nil {
		log.Fatalf("uploadToS3: %v", err)
	}
	log.Println("Upload complete")

	controllerURL := getenv("CONTROLLER_URL", "")
	if controllerURL == "" {
		log.Fatal("CONTROLLER_URL required")
	}
	buildToken := os.Getenv("BUILD_CONTROLLER_TOKEN")

	if *asyncMode {
		buildAsync(ctx, controllerURL, buildToken, serviceBuildConfigs, object)
	} else {
		buildSync(ctx, controllerURL, buildToken, serviceBuildConfigs, object)
	}
}

func buildSync(ctx context.Context, controllerURL, buildToken string, serviceBuildConfigs []ServiceBuildConfig, object string) {
	log.Printf("Building %d services synchronously", len(serviceBuildConfigs))

	for i, sbc := range serviceBuildConfigs {
		serviceName := sbc.ServiceName
		if serviceName == "" {
			serviceName = "default"
		}

		log.Printf("\n=== Build %d/%d: Service %s (architectures: %d) ===",
			i+1, len(serviceBuildConfigs), serviceName, len(sbc.Config.Bake))

		yamlBytes, err := yaml.Marshal(sbc.Config)
		if err != nil {
			log.Fatalf("marshal config for %s: %v", serviceName, err)
		}

		buildID, err := submitBuild(controllerURL, buildToken, object, yamlBytes, sbc.ServiceName)
		if err != nil {
			log.Fatalf("submit build for %s: %v", serviceName, err)
		}

		log.Printf("Build started for %s. ID=%s", serviceName, buildID)

		if err = streamLogs(controllerURL, buildID, buildToken); err != nil {
			log.Fatalf("Build failed for %s: %v", serviceName, err)
			os.Exit(1)
		}

		log.Printf("Service %s completed", serviceName)
	}

	log.Println("\nAll builds completed successfully")
}

func buildAsync(ctx context.Context, controllerURL, buildToken string, serviceBuildConfigs []ServiceBuildConfig, object string) {
	log.Printf("Building %d services asynchronously", len(serviceBuildConfigs))

	var wg sync.WaitGroup
	results := make(chan buildResult, len(serviceBuildConfigs))

	for _, sbc := range serviceBuildConfigs {
		wg.Add(1)
		go func(s ServiceBuildConfig) {
			defer wg.Done()

			serviceName := s.ServiceName
			if serviceName == "" {
				serviceName = "default"
			}

			log.Printf("[%s] Starting build (architectures: %d)", serviceName, len(s.Config.Bake))

			yamlBytes, err := yaml.Marshal(s.Config)
			if err != nil {
				results <- buildResult{
					ServiceName: serviceName,
					Error:       fmt.Errorf("marshal config: %w", err),
				}
				return
			}

			buildID, err := submitBuild(controllerURL, buildToken, object, yamlBytes, s.ServiceName)
			if err != nil {
				results <- buildResult{
					ServiceName: serviceName,
					Error:       fmt.Errorf("submit build: %w", err),
				}
				return
			}

			log.Printf("[%s] Build started. ID=%s", serviceName, buildID)

			if err = streamLogs(controllerURL, buildID, buildToken); err != nil {
				results <- buildResult{
					ServiceName: serviceName,
					Error:       fmt.Errorf("build failed: %w", err),
				}
				return
			}

			log.Printf("[%s] Build completed", serviceName)
			results <- buildResult{ServiceName: serviceName}
		}(sbc)
	}

	wg.Wait()
	close(results)

	var failed []buildResult
	for r := range results {
		if r.Error != nil {
			failed = append(failed, r)
			log.Printf("ERROR [%s]: %v", r.ServiceName, r.Error)
		}
	}

	if len(failed) > 0 {
		log.Fatalf("\n%d/%d services failed", len(failed), len(serviceBuildConfigs))
	}

	log.Println("\nAll services completed successfully")
}

func submitBuild(controllerURL, buildToken, object string, yamlBytes []byte, serviceName string) (string, error) {
	urlStr := fmt.Sprintf("%s/build?context_key=%s", controllerURL, url.QueryEscape(object))

	if serviceName != "" {
		urlStr += fmt.Sprintf("&service_name=%s", url.QueryEscape(serviceName))
	}

	req, _ := http.NewRequest("POST", urlStr, bytes.NewReader(yamlBytes))
	req.Header.Set("Content-Type", "application/x-yaml")
	if buildToken != "" {
		req.Header.Set("X-Build-Token", buildToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status=%s body=%s", resp.Status, string(b))
	}

	var br buildResponse
	if err = json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return "", err
	}

	return br.BuildID, nil
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func streamLogs(baseURL, buildID, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/build/%s/logs", baseURL, buildID),
		nil,
	)

	if token != "" {
		req.Header.Set("X-Build-Token", token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%s body=%s", resp.Status, string(b))
	}

	logFormat := getenv("LOG_FORMAT", "simple")
	reader := bufio.NewReader(resp.Body)
	buildFailed := false

	for {
		var line []byte
		line, err = reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read error: %w", err)
		}

		switch logFormat {
		case "simple":
			var entry logEntry
			if err = json.Unmarshal(line, &entry); err == nil {
				fmt.Println(entry.Message)
				if strings.Contains(entry.Message, "BUILD FAILED") {
					buildFailed = true
				}
				if entry.Level == "error" && strings.Contains(entry.Message, "build failed:") {
					buildFailed = true
				}
			} else {
				fmt.Print(string(line))
			}

		case "plain":
			var entry logEntry
			if err = json.Unmarshal(line, &entry); err == nil {
				plainMsg := ansiRegex.ReplaceAllString(entry.Message, "")
				fmt.Println(plainMsg)
				if strings.Contains(entry.Message, "BUILD FAILED") {
					buildFailed = true
				}
				if entry.Level == "error" && strings.Contains(entry.Message, "build failed:") {
					buildFailed = true
				}
			} else {
				plainLine := ansiRegex.ReplaceAllString(string(line), "")
				fmt.Print(plainLine)
			}

		default:
			fmt.Print(string(line))
			var entry logEntry
			if err = json.Unmarshal(line, &entry); err == nil {
				if strings.Contains(entry.Message, "BUILD FAILED") {
					buildFailed = true
				}
				if entry.Level == "error" && strings.Contains(entry.Message, "build failed:") {
					buildFailed = true
				}
			}
		}
	}

	if buildFailed {
		return fmt.Errorf("build failed")
	}

	return nil
}
