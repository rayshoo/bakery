package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var version = "dev"

const (
	colorReset = "\033[0m"
	colorRed   = "\033[31m"
	colorGreen = "\033[32m"
	colorCyan  = "\033[36m"
)

var taskColors = []string{
	"\033[34m",
	"\033[35m",
	"\033[36m",
	"\033[33m",
	"\033[32m",
	"\033[91m",
	"\033[92m",
	"\033[93m",
	"\033[94m",
	"\033[95m",
}

// AgentResult 는 빌드 결과를 controller에 전송하기 위한 구조체 입니다.
type AgentResult struct {
	TaskID      string `json:"taskId"`
	Arch        string `json:"arch"`
	ImageDigest string `json:"imageDigest"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getTaskColor 는 taskID에 해당하는 터미널 색상 코드를 반환합니다.
func getTaskColor(taskID string) string {
	if colorIdx := os.Getenv("TASK_COLOR_INDEX"); colorIdx != "" {
		if idx, err := strconv.Atoi(colorIdx); err == nil {
			return taskColors[idx%len(taskColors)]
		}
	}

	if idx := strings.LastIndex(taskID, "-"); idx != -1 {
		if num, err := strconv.Atoi(taskID[idx+1:]); err == nil {
			return taskColors[num%len(taskColors)]
		}
	}
	return taskColors[0]
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	buildID := os.Getenv("BUILD_ID")
	controllerURL := os.Getenv("CONTROLLER_URL")
	taskID := os.Getenv("BUILD_TASK_ID")

	if taskID == "" {
		taskID = "unknown"
	}

	targetArch := os.Getenv("TARGETARCH")
	if targetArch == "" {
		targetArch = "unknown"
	}

	executorPlatform := getenv("EXECUTOR_PLATFORM", "ecs")

	if buildID == "" || controllerURL == "" {
		log.Fatalf("missing required env: BUILD_ID / CONTROLLER_URL")
	}

	ingestURL := fmt.Sprintf("%s/build/%s/logs/ingest?task=%s", controllerURL, buildID, taskID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	req, w, pw := newStreamingRequest("POST", ingestURL)
	req = req.WithContext(ctx)
	req.TransferEncoding = []string{"chunked"}
	req.ContentLength = -1

	tr := &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
		DisableCompression: true,
		MaxIdleConns:       1,
		IdleConnTimeout:    120 * time.Minute,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   0,
	}

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)

	go func() {
		log.Println("[agent] trying ingest connect:", ingestURL)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[agent] ingest connect error: %v\n", err)
			errCh <- err
			return
		}
		log.Printf("[agent] ingest connected: %s\n", resp.Status)
		respCh <- resp
	}()

	var logMu sync.Mutex
	stopKeepalive := make(chan struct{})
	defer close(stopKeepalive)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				logMu.Lock()
				_, _ = w.WriteString("\n")
				_ = w.Flush()
				logMu.Unlock()
			case <-stopKeepalive:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	taskColor := getTaskColor(taskID)

	logLine := func(step, level, msg string) {
		line := fmt.Sprintf("%s[%s][%s]%s %s: %s",
			taskColor, executorPlatform, taskID, colorReset, step, msg)
		log.Println(line)

		logMu.Lock()
		_, _ = w.WriteString(line + "\n")
		_ = w.Flush()
		logMu.Unlock()
	}

	exitCode := 0
	var imageDigest string

	fail := func(step string, err error) {
		logLine(step, "error", fmt.Sprintf("%serror:%s %s", colorRed, colorReset, err.Error()))
		exitCode = 1
	}

	exitWithFlush := func() {
		logLine("agent", "error", fmt.Sprintf("agent exiting with code %d", exitCode))

		result := AgentResult{
			TaskID:      taskID,
			Arch:        targetArch,
			ImageDigest: imageDigest,
			Success:     exitCode == 0,
		}
		if exitCode != 0 {
			result.Error = "build failed"
		}
		_ = sendResult(controllerURL, buildID, taskID, result)

		closeWrite(w, pw)
		if err := waitResponse(respCh, errCh); err != nil {
			logLine("agent", "error", fmt.Sprintf("ingest response error: %v", err))
		}
		os.Exit(exitCode)
	}

	contextBucket := os.Getenv("CONTEXT_BUCKET")
	contextKey := os.Getenv("CONTEXT_KEY")
	if contextBucket == "" || contextKey == "" {
		fail("init", fmt.Errorf("missing CONTEXT_BUCKET or CONTEXT_KEY"))
		exitWithFlush()
	}

	if err := os.MkdirAll("/tmp", 0755); err != nil {
		fail("init", fmt.Errorf("create temporary dir: %w", err))
		exitWithFlush()
	}

	if err := runStep(ctx, "download", logLine, func(ctx context.Context, logf func(string)) error {
		endpoint := normalizeEndpoint(os.Getenv("STORAGE_ENDPOINT"))
		region := getenv("STORAGE_REGION", "us-east-1")
		useSSL := getenv("STORAGE_USE_SSL", "true") == "true"

		s3Client, err := newS3Client(ctx, endpoint, region, useSSL)
		if err != nil {
			return fmt.Errorf("create s3 client: %w", err)
		}

		logf(fmt.Sprintf("downloading s3://%s/%s", contextBucket, contextKey))

		obj, err := s3Client.GetObject(ctx, contextBucket, contextKey, minio.GetObjectOptions{})
		if err != nil {
			return fmt.Errorf("get object: %w", err)
		}
		defer obj.Close()

		outFile, err := os.Create("/tmp/context.tar.gz")
		if err != nil {
			return fmt.Errorf("create file: %w", err)
		}
		defer outFile.Close()

		written, err := io.Copy(outFile, obj)
		if err != nil {
			return fmt.Errorf("copy object: %w", err)
		}

		logf(fmt.Sprintf("downloaded %d bytes", written))
		return nil
	}); err != nil {
		fail("download", err)
		exitWithFlush()
	}

	if err := runStep(ctx, "extract", logLine, func(ctx context.Context, logf func(string)) error {
		if err := os.MkdirAll("/workspace", 0755); err != nil {
			return fmt.Errorf("create workspace dir: %w", err)
		}
		logf("extracting /tmp/context.tar.gz to /workspace")
		return runCmdStreaming(ctx, "tar", []string{"-xzf", "/tmp/context.tar.gz", "-C", "/workspace"}, logf)
	}); err != nil {
		fail("extract", err)
		exitWithFlush()
	}

	if err := runStep(ctx, "docker-config", logLine, func(ctx context.Context, logf func(string)) error {
		credsJSON := os.Getenv("KANIKO_CREDENTIALS_JSON")
		if credsJSON == "" {
			logf("no kaniko credentials provided, skipping")
			return nil
		}

		dockerDir := "/kaniko/.docker"
		if err := os.MkdirAll(dockerDir, 0755); err != nil {
			return fmt.Errorf("create .docker dir: %w", err)
		}

		configPath := dockerDir + "/config.json"
		if err := os.WriteFile(configPath, []byte(credsJSON), 0600); err != nil {
			return fmt.Errorf("write config.json: %w", err)
		}

		logf(fmt.Sprintf("wrote docker config to %s", configPath))
		return nil
	}); err != nil {
		fail("docker-config", err)
		exitWithFlush()
	}

	preScript := os.Getenv("PRE_SCRIPT")
	if preScript != "" {
		if err := runStep(ctx, "pre", logLine, func(ctx context.Context, logf func(string)) error {
			logf(preScript)
			cmd := exec.CommandContext(ctx, "sh", "-ce", preScript)
			cmd.Dir = "/"
			return attachStreaming(cmd, logf)
		}); err != nil {
			fail("pre", err)
			exitWithFlush()
		}
	}

	if err := runStep(ctx, "kaniko", logLine, func(ctx context.Context, logf func(string)) error {
		kanikoContext := getenv("KANIKO_CONTEXT", ".")
		kanikoDockerfile := getenv("KANIKO_DOCKERFILE", "Dockerfile")
		kanikoDestination := os.Getenv("KANIKO_DESTINATION")

		if kanikoDestination == "" {
			return fmt.Errorf("KANIKO_DESTINATION not set")
		}

		args := []string{
			fmt.Sprintf("--context=/workspace/%s", kanikoContext),
			fmt.Sprintf("--dockerfile=%s", kanikoDockerfile),
			fmt.Sprintf("--destination=%s", kanikoDestination),
			"--digest-file=/tmp/image-digest",
		}

		customBuildArgs := make(map[string]string)
		if customArgs := os.Getenv("KANIKO_BUILD_ARGS"); customArgs != "" {
			for _, pair := range strings.Split(customArgs, ",") {
				if pair != "" {
					parts := strings.SplitN(pair, "=", 2)
					if len(parts) == 2 {
						customBuildArgs[parts[0]] = parts[1]
					}
				}
			}
		}

		if _, exists := customBuildArgs["TARGETPLATFORM"]; !exists {
			if v := os.Getenv("TARGETPLATFORM"); v != "" {
				args = append(args, fmt.Sprintf("--build-arg=TARGETPLATFORM=%s", v))
			}
		}
		if _, exists := customBuildArgs["TARGETOS"]; !exists {
			if v := os.Getenv("TARGETOS"); v != "" {
				args = append(args, fmt.Sprintf("--build-arg=TARGETOS=%s", v))
			}
		}
		if _, exists := customBuildArgs["TARGETARCH"]; !exists {
			if v := os.Getenv("TARGETARCH"); v != "" {
				args = append(args, fmt.Sprintf("--build-arg=TARGETARCH=%s", v))
			}
		}
		if _, exists := customBuildArgs["TARGETVARIANT"]; !exists {
			if v := os.Getenv("TARGETVARIANT"); v != "" {
				args = append(args, fmt.Sprintf("--build-arg=TARGETVARIANT=%s", v))
			}
		}

		buildPlatform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
		if _, exists := customBuildArgs["BUILDPLATFORM"]; !exists {
			args = append(args, fmt.Sprintf("--build-arg=BUILDPLATFORM=%s", buildPlatform))
		}
		if _, exists := customBuildArgs["BUILDOS"]; !exists {
			args = append(args, fmt.Sprintf("--build-arg=BUILDOS=%s", runtime.GOOS))
		}
		if _, exists := customBuildArgs["BUILDARCH"]; !exists {
			args = append(args, fmt.Sprintf("--build-arg=BUILDARCH=%s", runtime.GOARCH))
		}

		for key, value := range customBuildArgs {
			args = append(args, fmt.Sprintf("--build-arg=%s=%s", key, value))
		}

		if getenv("KANIKO_CACHE_ENABLE", "false") == "true" {
			args = append(args, "--cache=true")
			if repo := os.Getenv("KANIKO_CACHE_REPO"); repo != "" {
				args = append(args, fmt.Sprintf("--cache-repo=%s", repo))
			}
			if ttl := os.Getenv("KANIKO_CACHE_TTL"); ttl != "" {
				args = append(args, fmt.Sprintf("--cache-ttl=%s", ttl))
			}
			if getenv("KANIKO_CACHE_COPY_LAYERS", "false") == "true" {
				args = append(args, "--cache-copy-layers")
			}
			if getenv("KANIKO_CACHE_RUN_LAYERS", "false") == "true" {
				args = append(args, "--cache-run-layers")
			}
			if getenv("KANIKO_CACHE_COMPRESSED", "false") == "true" {
				args = append(args, "--compressed-caching=true")
			}
		}

		if mode := os.Getenv("KANIKO_SNAPSHOT_MODE"); mode != "" {
			args = append(args, fmt.Sprintf("--snapshot-mode=%s", mode))
		}

		if getenv("KANIKO_USE_NEW_RUN", "false") == "true" {
			args = append(args, "--use-new-run")
		}

		if getenv("KANIKO_CLEANUP", "false") == "true" {
			args = append(args, "--cleanup")
		}

		if platform := os.Getenv("KANIKO_CUSTOM_PLATFORM"); platform != "" {
			args = append(args, fmt.Sprintf("--custom-platform=%s", platform))
		}

		if getenv("KANIKO_NO_PUSH", "false") == "true" {
			args = append(args, "--no-push")
		}

		ignorePathsEnv := os.Getenv("KANIKO_IGNORE_PATH")
		ignorePaths := make([]string, 0, 4)
		seenIgnore := map[string]bool{}

		for _, path := range strings.Split(ignorePathsEnv, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if !seenIgnore[path] {
				ignorePaths = append(ignorePaths, path)
				seenIgnore[path] = true
			}
		}
		if !seenIgnore["/workspace"] {
			ignorePaths = append(ignorePaths, "/workspace")
		}
		for _, path := range ignorePaths {
			args = append(args, fmt.Sprintf("--ignore-path=%s", path))
		}

		if extraFlags := os.Getenv("KANIKO_EXTRA_FLAGS"); extraFlags != "" {
			extraArgs := strings.Fields(extraFlags)
			args = append(args, extraArgs...)
		}

		logf(fmt.Sprintf("running: /kaniko/executor %s", strings.Join(args, " ")))
		if err := runCmdStreaming(ctx, "/kaniko/executor", args, logf); err != nil {
			return err
		}

		if getenv("KANIKO_NO_PUSH", "false") == "true" {
			logf("no-push mode: skipping digest read")
			imageDigest = "no-push"
			return nil
		}

		digestBytes, err := os.ReadFile("/tmp/image-digest")
		if err != nil {
			return fmt.Errorf("read digest file: %w", err)
		}
		imageDigest = strings.TrimSpace(string(digestBytes))
		logf(fmt.Sprintf("image digest: %s", imageDigest))

		return nil
	}); err != nil {
		fail("kaniko", err)
		exitWithFlush()
	}

	postScript := os.Getenv("POST_SCRIPT")
	if postScript != "" {
		if err := runStep(ctx, "post", logLine, func(ctx context.Context, logf func(string)) error {
			logf(postScript)
			cmd := exec.CommandContext(ctx, "sh", "-ce", postScript)
			cmd.Dir = "/"
			return attachStreaming(cmd, logf)
		}); err != nil {
			fail("post", err)
			exitWithFlush()
		}
	}

	logLine("agent", "info", fmt.Sprintf("%ssuccess:%s build completed", colorGreen, colorReset))

	result := AgentResult{
		TaskID:      taskID,
		Arch:        targetArch,
		ImageDigest: imageDigest,
		Success:     true,
	}
	if err := sendResult(controllerURL, buildID, taskID, result); err != nil {
		logLine("agent", "error", fmt.Sprintf("failed to send result: %v", err))
	}

	closeWrite(w, pw)
	if err := waitResponse(respCh, errCh); err != nil {
		logLine("agent", "error", fmt.Sprintf("ingest response error: %v", err))
	}
}

func sendResult(baseURL, buildID, taskID string, result AgentResult) error {
	url := fmt.Sprintf("%s/build/%s/result?task=%s", baseURL, buildID, taskID)
	body, _ := json.Marshal(result)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("result post failed: %s %s", resp.Status, string(b))
	}
	return nil
}

func newS3Client(ctx context.Context, endpoint, region string, useSSL bool) (*minio.Client, error) {
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
		useSSL = true
	}

	accessKey := getenv("STORAGE_ACCESS_KEY", "")
	secretKey := getenv("STORAGE_SECRET_KEY", "")
	sessionToken := getenv("STORAGE_SESSION_TOKEN", "")

	if accessKey != "" && secretKey != "" {
		return minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(accessKey, secretKey, sessionToken),
			Region: region,
			Secure: useSSL,
		})
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	creds, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve aws credentials: %w", err)
	}

	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken),
		Region: region,
		Secure: useSSL,
	})
}

func newStreamingRequest(method, url string) (*http.Request, *bufio.Writer, *io.PipeWriter) {
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(method, url, pr)
	req.Header.Set("Content-Type", "text/plain")
	w := bufio.NewWriter(pw)
	return req, w, pw
}

func closeWrite(w *bufio.Writer, pw *io.PipeWriter) {
	_ = w.Flush()
	_ = pw.Close()
}

func waitResponse(respCh <-chan *http.Response, errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	case resp := <-respCh:
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("ingest status=%s body=%s", resp.Status, string(body))
		}
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("ingest response timeout")
	}
}

func runStep(
	ctx context.Context,
	step string,
	logLine func(step, level, msg string),
	fn func(ctx context.Context, logf func(string)) error,
) error {
	logF := func(msg string) {
		logLine(step, "info", msg)
	}

	logF(fmt.Sprintf("%sstart%s", colorCyan, colorReset))
	err := fn(ctx, logF)
	if err != nil {
		logLine(step, "error", err.Error())
		return err
	}
	logF(fmt.Sprintf("%sdone%s", colorGreen, colorReset))
	return nil
}

func runCmdStreaming(ctx context.Context, name string, args []string, logf func(string)) error {
	cmd := exec.CommandContext(ctx, name, args...)
	return attachStreaming(cmd, logf)
}

func attachStreaming(cmd *exec.Cmd, logf func(string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			logf(sc.Text())
		}
	}()

	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logf(sc.Text())
		}
	}()

	wg.Wait()
	return cmd.Wait()
}

func normalizeEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ""
	}
	if ep == "s3.amazonaws.com" {
		return ""
	}
	ep = strings.TrimPrefix(ep, "http://")
	ep = strings.TrimPrefix(ep, "https://")
	return ep
}
