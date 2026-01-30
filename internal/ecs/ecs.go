package ecs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rayshoo/bakery/internal/config"
	"github.com/rayshoo/bakery/internal/state"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ECSExecutor runs build tasks on AWS ECS Fargate.
type ECSExecutor struct {
	Client            *awsecs.Client
	ClusterName       string
	AWSRegion         string
	AgentImage        string
	ExecutionRole     string
	TaskRole          string
	SubnetIDs         []string
	SecurityGroupIDs  []string
	RegistrySecretARN string
	ControllerURL     string

	taskDefMu    sync.Mutex
	taskDefCache map[string]bool
}

// NewECSExecutor creates a new ECSExecutor instance.
func NewECSExecutor(
	client *awsecs.Client,
	cluster string,
	agentImage string,
	execRole string,
	taskRole string,
	subnets []string,
	sg []string,
	region string,
	registrySecretArn string,
	controllerURL string,
) *ECSExecutor {
	return &ECSExecutor{
		Client:            client,
		ClusterName:       cluster,
		AgentImage:        agentImage,
		ExecutionRole:     execRole,
		TaskRole:          taskRole,
		SubnetIDs:         subnets,
		SecurityGroupIDs:  sg,
		AWSRegion:         region,
		RegistrySecretARN: registrySecretArn,
		ControllerURL:     controllerURL,
		taskDefCache:      make(map[string]bool),
	}
}

// RunTask runs an ECS task for the specified architecture.
func (e *ECSExecutor) RunTask(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	ef config.EffectiveConfig,
	bucket string,
	key string,
	ingestURL string,
) error {
	if ef.Arch == "" {
		return errors.New("ECSExecutor.RunTask: missing arch")
	}

	st.AppendLog("info", fmt.Sprintf("[task %s] dispatch arch=%s", taskID, ef.Arch))

	return e.RunTaskForArch(ctx, st, taskID, ef, bucket, key, ingestURL, st.IsSingleArch, st.GlobalDestination)
}

func validateECSResources(cpu, memory string) error {
	validCombinations := map[string][]string{
		"256":   {"512", "1024", "2048"},
		"512":   {"1024", "2048", "3072", "4096"},
		"1024":  {"2048", "3072", "4096", "5120", "6144", "7168", "8192"},
		"2048":  {"4096", "5120", "6144", "7168", "8192", "9216", "10240", "11264", "12288", "13312", "14336", "15360", "16384"},
		"4096":  {"8192", "9216", "10240", "11264", "12288", "13312", "14336", "15360", "16384", "17408", "18432", "19456", "20480", "21504", "22528", "23552", "24576", "25600", "26624", "27648", "28672", "29696", "30720"},
		"8192":  {"16384", "20480", "24576", "28672", "32768", "36864", "40960", "45056", "49152", "53248", "57344", "61440"},
		"16384": {"32768", "40960", "49152", "57344", "65536", "73728", "81920", "90112", "98304", "106496", "114688", "122880"},
	}

	validMemories, ok := validCombinations[cpu]
	if !ok {
		return fmt.Errorf("invalid ECS CPU: %s", cpu)
	}

	for _, validMem := range validMemories {
		if memory == validMem {
			return nil
		}
	}

	return fmt.Errorf("invalid ECS CPU/Memory combination: CPU=%s Memory=%s", cpu, memory)
}

// EnsureTaskDefinitionForArch checks if a Task Definition exists for the given architecture
// and resource settings, creating one if needed. Uses a mutex to prevent concurrent creation.
func (e *ECSExecutor) EnsureTaskDefinitionForArch(ctx context.Context, arch string, cpu string, memory string) (string, error) {
	if cpu == "" {
		cpu = "256"
	}
	if memory == "" {
		memory = "512"
	}

	cpuNorm, memNorm, err := config.NormalizeECSResources(cpu, memory)
	if err != nil {
		return "", fmt.Errorf("normalize resources: %w", err)
	}

	if err := validateECSResources(cpuNorm, memNorm); err != nil {
		return "", err
	}

	family := fmt.Sprintf("%s-%s-%s-%s", getenv("AGENT_TASK_FAMILY", "build-agent"), arch, cpuNorm, memNorm)

	e.taskDefMu.Lock()
	defer e.taskDefMu.Unlock()

	if e.taskDefCache[family] {
		return family, nil
	}

	_, err = e.Client.DescribeTaskDefinition(ctx, &awsecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(family),
	})
	if err == nil {
		e.taskDefCache[family] = true
		return family, nil
	}

	var cpuArch ecstypes.CPUArchitecture
	switch arch {
	case "amd64":
		cpuArch = ecstypes.CPUArchitectureX8664
	case "arm64":
		cpuArch = ecstypes.CPUArchitectureArm64
	default:
		return "", fmt.Errorf("unknown arch: %s", arch)
	}

	log.Printf("[ECS] Creating TaskDefinition for arch=%s cpu=%s memory=%s", arch, cpuNorm, memNorm)

	container := ecstypes.ContainerDefinition{
		Name:      aws.String("agent"),
		Image:     aws.String(e.AgentImage),
		Essential: aws.Bool(true),
	}

	if e.RegistrySecretARN != "" {
		container.RepositoryCredentials = &ecstypes.RepositoryCredentials{
			CredentialsParameter: aws.String(e.RegistrySecretARN),
		}
	}

	e.applyLogConfig(&container)

	input := &awsecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(family),
		Cpu:                     aws.String(cpuNorm),
		Memory:                  aws.String(memNorm),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		ExecutionRoleArn:        aws.String(e.ExecutionRole),
		TaskRoleArn:             aws.String(e.TaskRole),
		RuntimePlatform: &ecstypes.RuntimePlatform{
			CpuArchitecture:       cpuArch,
			OperatingSystemFamily: ecstypes.OSFamilyLinux,
		},
		ContainerDefinitions: []ecstypes.ContainerDefinition{container},
	}

	out, err := e.Client.RegisterTaskDefinition(ctx, input)
	if err != nil {
		if strings.Contains(err.Error(), "Too many concurrent attempts") ||
			strings.Contains(err.Error(), "ResourceInUseException") {
			log.Printf("[ECS] Task definition %s already being created or exists, retrying...", family)

			time.Sleep(500 * time.Millisecond)

			_, err = e.Client.DescribeTaskDefinition(ctx, &awsecs.DescribeTaskDefinitionInput{
				TaskDefinition: aws.String(family),
			})
			if err == nil {
				e.taskDefCache[family] = true
				log.Printf("[ECS] Task definition %s confirmed to exist", family)
				return family, nil
			}
		}
		return "", fmt.Errorf("register taskdef: %w", err)
	}

	arn := aws.ToString(out.TaskDefinition.TaskDefinitionArn)
	log.Printf("[ECS] Created TaskDefinition arch=%s cpu=%s memory=%s arn=%s", arch, cpuNorm, memNorm, arn)

	e.taskDefCache[family] = true

	return family, nil
}

// RunTaskForArch runs an ECS task for a specific architecture and waits for completion.
func (e *ECSExecutor) RunTaskForArch(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	ef config.EffectiveConfig,
	bucket string,
	key string,
	ingestURL string,
	isSingleArch bool,
	globalDestination string,
) error {
	arch := ef.Arch

	tdFamily, err := e.EnsureTaskDefinitionForArch(ctx, arch, ef.CPU, ef.Memory)
	if err != nil {
		return err
	}

	st.AppendLog("info", fmt.Sprintf("[ecs][%s] task definition = %s (cpu=%s memory=%s)", taskID, tdFamily, ef.CPU, ef.Memory))

	var targetPlatform, targetOS, targetArch, targetVariant string

	if ef.CustomPlatform != nil && *ef.CustomPlatform != "" {
		targetPlatform = *ef.CustomPlatform
		parts := strings.Split(*ef.CustomPlatform, "/")
		if len(parts) >= 2 {
			targetOS = parts[0]
			targetArch = parts[1]
			if len(parts) == 3 {
				targetVariant = parts[2]
			}
		} else {
			targetOS = "linux"
			targetArch = arch
		}
	} else {
		targetPlatform = fmt.Sprintf("linux/%s", arch)
		targetOS = "linux"
		targetArch = arch
		if arch == "arm64" {
			targetVariant = "v8"
		}
	}

	var kanikoDestination string

	if isSingleArch {
		if ef.Destination != "" {
			kanikoDestination = ef.Destination
		} else {
			kanikoDestination = globalDestination
		}
	} else {
		if ef.Destination != "" && ef.Destination != globalDestination {
			kanikoDestination = ef.Destination
		} else {
			if st.HasDuplicateArch {
				kanikoDestination = appendTaskSuffix(globalDestination, taskID)
			} else {
				kanikoDestination = appendArchSuffix(globalDestination, arch)
			}
		}
	}

	var kanikoCredsJSON string
	if len(ef.KanikoCredentials) > 0 {
		creds, err := createDockerConfigJSON(ef.KanikoCredentials)
		if err != nil {
			return fmt.Errorf("create docker config: %w", err)
		}
		kanikoCredsJSON = creds
	}

	var buildArgsStr string
	if len(ef.BuildArgs) > 0 {
		var pairs []string
		for k, v := range ef.BuildArgs {
			pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
		}
		buildArgsStr = strings.Join(pairs, ",")
	}

	env := []ecstypes.KeyValuePair{
		kv("BUILD_ID", st.ID),
		kv("BUILD_TASK_ID", taskID),
		kv("TASK_COLOR_INDEX", getTaskColorIndex(taskID)),

		kv("TARGETPLATFORM", targetPlatform),
		kv("TARGETOS", targetOS),
		kv("TARGETARCH", targetArch),
		kv("TARGETVARIANT", targetVariant),

		kv("EXECUTOR_PLATFORM", "ecs"),

		kv("STORAGE_ENDPOINT", os.Getenv("S3_ENDPOINT")),
		kv("STORAGE_REGION", os.Getenv("S3_REGION")),
		kv("STORAGE_USE_SSL", os.Getenv("S3_SSL")),
		kv("STORAGE_ACCESS_KEY", os.Getenv("S3_ACCESS_KEY")),
		kv("STORAGE_SECRET_KEY", os.Getenv("S3_SECRET_KEY")),

		kv("CONTEXT_BUCKET", bucket),
		kv("CONTEXT_KEY", key),

		kv("CONTROLLER_URL", e.ControllerURL),
		kv("INGEST_URL", ingestURL),

		kv("KANIKO_DESTINATION", kanikoDestination),
		kv("KANIKO_CONTEXT", ef.ContextPath),
		kv("KANIKO_DOCKERFILE", ef.Dockerfile),
		kv("KANIKO_BUILD_ARGS", buildArgsStr),
		kv("KANIKO_CREDENTIALS_JSON", kanikoCredsJSON),
	}

	if ef.CacheEnable != nil {
		env = append(env, kv("KANIKO_CACHE_ENABLE", fmt.Sprintf("%t", *ef.CacheEnable)))
	}
	if ef.CacheRepo != "" {
		env = append(env, kv("KANIKO_CACHE_REPO", ef.CacheRepo))
	}
	if ef.CacheTTL != "" {
		env = append(env, kv("KANIKO_CACHE_TTL", ef.CacheTTL))
	}
	if ef.CacheCopyLayers != nil {
		env = append(env, kv("KANIKO_CACHE_COPY_LAYERS", fmt.Sprintf("%t", *ef.CacheCopyLayers)))
	}
	if ef.CacheRunLayers != nil {
		env = append(env, kv("KANIKO_CACHE_RUN_LAYERS", fmt.Sprintf("%t", *ef.CacheRunLayers)))
	}
	if ef.CacheCompressed != nil {
		env = append(env, kv("KANIKO_CACHE_COMPRESSED", fmt.Sprintf("%t", *ef.CacheCompressed)))
	}

	if ef.SnapshotMode != nil {
		env = append(env, kv("KANIKO_SNAPSHOT_MODE", *ef.SnapshotMode))
	}
	if ef.UseNewRun != nil {
		env = append(env, kv("KANIKO_USE_NEW_RUN", fmt.Sprintf("%t", *ef.UseNewRun)))
	}
	if ef.Cleanup != nil {
		env = append(env, kv("KANIKO_CLEANUP", fmt.Sprintf("%t", *ef.Cleanup)))
	}
	if ef.CustomPlatform != nil {
		env = append(env, kv("KANIKO_CUSTOM_PLATFORM", *ef.CustomPlatform))
	}
	if ef.NoPush != nil {
		env = append(env, kv("KANIKO_NO_PUSH", fmt.Sprintf("%t", *ef.NoPush)))
	}

	if len(ef.IgnorePath) > 0 {
		env = append(env, kv("KANIKO_IGNORE_PATH", strings.Join(ef.IgnorePath, ",")))
	}

	if ef.ExtraFlags != "" {
		env = append(env, kv("KANIKO_EXTRA_FLAGS", ef.ExtraFlags))
	}

	if ef.PreScript != nil {
		env = append(env, kv("PRE_SCRIPT", *ef.PreScript))
	}
	if ef.PostScript != nil {
		env = append(env, kv("POST_SCRIPT", *ef.PostScript))
	}

	for k, v := range ef.Env {
		env = append(env, kv(k, v))
	}

	runOut, err := e.Client.RunTask(ctx, &awsecs.RunTaskInput{
		Cluster:        aws.String(e.ClusterName),
		TaskDefinition: aws.String(tdFamily),
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          aws.Int32(1),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        e.SubnetIDs,
				SecurityGroups: e.SecurityGroupIDs,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{
				{
					Name:        aws.String("agent"),
					Environment: env,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("RunTask: %w", err)
	}
	if len(runOut.Tasks) == 0 {
		return fmt.Errorf("RunTask returned no tasks")
	}

	taskArn := aws.ToString(runOut.Tasks[0].TaskArn)

	st.Mu.Lock()
	st.TaskArnByID[taskID] = taskArn
	st.IDByTaskArn[taskArn] = taskID
	st.Mu.Unlock()

	st.AppendLog("info", fmt.Sprintf("[ecs][%s] started task: %s", taskID, taskArn))

	go e.StreamTaskLogs(ctx, st, taskArn, taskID)

	if err := e.waitTaskStopped(ctx, st, taskID, taskArn); err != nil {
		return err
	}

	return e.checkTaskExitCode(st, taskArn)
}

func kv(k, v string) ecstypes.KeyValuePair {
	return ecstypes.KeyValuePair{
		Name:  aws.String(k),
		Value: aws.String(v),
	}
}

func (e *ECSExecutor) applyLogConfig(c *ecstypes.ContainerDefinition) {
	logGroup := os.Getenv("ECS_LOG_GROUP")
	if logGroup == "" {
		return
	}
	region := e.AWSRegion
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}

	c.LogConfiguration = &ecstypes.LogConfiguration{
		LogDriver: ecstypes.LogDriverAwslogs,
		Options: map[string]string{
			"awslogs-group":         logGroup,
			"awslogs-region":        region,
			"awslogs-stream-prefix": "agent",
		},
	}
}

func (e *ECSExecutor) waitTaskStopped(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	taskArn string,
) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for ECS task: %w", ctx.Err())

		case <-time.After(3 * time.Second):
			out, err := e.Client.DescribeTasks(ctx, &awsecs.DescribeTasksInput{
				Cluster: aws.String(e.ClusterName),
				Tasks:   []string{taskArn},
			})
			if err != nil {
				st.AppendLog("error", fmt.Sprintf("[ecs][%s] describe error: %v", taskID, err))
				continue
			}

			if len(out.Tasks) == 0 {
				continue
			}

			t := out.Tasks[0]

			if t.LastStatus != nil {
				st.AppendLog("debug", fmt.Sprintf("[ecs][%s] status=%s", taskID, *t.LastStatus))
			}

			if t.LastStatus != nil && *t.LastStatus == "STOPPED" {
				return nil
			}
		}
	}
}

func (e *ECSExecutor) checkTaskExitCode(
	st *state.BuildState,
	taskArn string,
) error {
	st.Mu.RLock()
	taskID := st.IDByTaskArn[taskArn]
	st.Mu.RUnlock()

	if taskID == "" {
		taskID = "unknown"
	}

	out, err := e.Client.DescribeTasks(context.TODO(),
		&awsecs.DescribeTasksInput{
			Cluster: aws.String(e.ClusterName),
			Tasks:   []string{taskArn},
		},
	)

	if err != nil {
		st.AppendLog("error", fmt.Sprintf("[ecs][%s] DescribeTasks error: %v", taskID, err))
		st.SetError(err)
		return err
	}

	if len(out.Tasks) == 0 {
		err := fmt.Errorf("no task info")
		st.SetError(err)
		return err
	}

	t := out.Tasks[0]

	for _, c := range t.Containers {
		if c.Name != nil && *c.Name == "agent" {
			exit := aws.ToInt32(c.ExitCode)

			var taskErr error
			if exit != 0 {
				taskErr = fmt.Errorf("agent exit=%d", exit)
				st.SetError(taskErr)
				st.AppendLog("error", fmt.Sprintf("[ecs][%s] exit=%d", taskID, exit))
			} else {
				st.AppendLog("info", fmt.Sprintf("[ecs][%s] exit=0 success", taskID))
			}

			return taskErr
		}
	}

	err = fmt.Errorf("agent container not found")
	st.SetError(err)
	return err
}

// StreamTaskLogs streams logs from an ECS task.
// Currently empty as only ingest-based streaming is used.
func (e *ECSExecutor) StreamTaskLogs(
	ctx context.Context,
	st *state.BuildState,
	taskArn string,
	taskID string,
) {
}

func appendArchSuffix(destination, arch string) string {
	if idx := lastIndexByte(destination, ':'); idx != -1 {
		base := destination[:idx]
		tag := destination[idx+1:]
		return fmt.Sprintf("%s:%s_%s", base, tag, arch)
	}
	return fmt.Sprintf("%s:latest_%s", destination, arch)
}

func appendTaskSuffix(destination, taskID string) string {
	if idx := lastIndexByte(destination, ':'); idx != -1 {
		base := destination[:idx]
		tag := destination[idx+1:]
		return fmt.Sprintf("%s:%s_%s", base, tag, taskID)
	}
	return fmt.Sprintf("%s:latest_%s", destination, taskID)
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// DockerConfig holds Docker registry auth configuration for kaniko.
type DockerConfig struct {
	Auths map[string]DockerAuth `json:"auths"`
}

// DockerAuth holds Docker registry authentication credentials.
type DockerAuth struct {
	Auth string `json:"auth"`
}

func createDockerConfigJSON(creds []config.RegistryCredential) (string, error) {
	cfg := DockerConfig{
		Auths: make(map[string]DockerAuth),
	}

	for _, cred := range creds {
		auth := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password))
		cfg.Auths[cred.Registry] = DockerAuth{Auth: auth}
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

// getTaskColorIndex returns the terminal color index for a task ID.
// amd64 tasks use even indices, arm64 tasks use odd indices.
func getTaskColorIndex(taskID string) string {
	if taskID == "amd64" {
		return "0"
	}
	if taskID == "arm64" {
		return "1"
	}

	if strings.Contains(taskID, "-") {
		parts := strings.Split(taskID, "-")
		if len(parts) == 2 {
			arch := parts[0]
			idx := parts[1]

			num, err := strconv.Atoi(idx)
			if err != nil {
				return "0"
			}

			if arch == "amd64" {
				return strconv.Itoa(num * 2)
			} else if arch == "arm64" {
				return strconv.Itoa(num*2 + 1)
			}
		}
	}

	return "0"
}
