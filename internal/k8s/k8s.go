package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rayshoo/bakery/internal/config"
	"github.com/rayshoo/bakery/internal/state"

	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/client-go/kubernetes"
)

// K8sExecutor 는 Kubernetes 에서 빌드 Job 을 실행하는 executor 입니다.
type K8sExecutor struct {
	Client        *kubernetes.Clientset
	Namespace     string
	AgentImage    string
	ControllerURL string
	K8sConfig     *config.K8sServerConfig
}

// NewK8sExecutor 는 K8sExecutor 인스턴스를 생성합니다.
func NewK8sExecutor(
	client *kubernetes.Clientset,
	namespace string,
	agentImage string,
	controllerURL string,
	k8sConfig *config.K8sServerConfig,
) *K8sExecutor {
	return &K8sExecutor{
		Client:        client,
		Namespace:     namespace,
		AgentImage:    agentImage,
		ControllerURL: controllerURL,
		K8sConfig:     k8sConfig,
	}
}

// RunTask 는 Kubernetes Job 을 생성하여 빌드 태스크를 실행합니다.
func (k *K8sExecutor) RunTask(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	ef config.EffectiveConfig,
	contextBucket string,
	contextKey string,
	ingestURL string,
) error {

	arch := ef.Arch

	jobName := fmt.Sprintf("build-%s-%s-", st.ID, taskID)
	st.AppendLog("info", fmt.Sprintf("[k8s][%s] dispatching job", taskID))

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

	envVars := []apiv1.EnvVar{
		{Name: "BUILD_ID", Value: st.ID},
		{Name: "BUILD_TASK_ID", Value: taskID},
		{Name: "TASK_COLOR_INDEX", Value: getTaskColorIndex(taskID)},

		{Name: "TARGETPLATFORM", Value: targetPlatform},
		{Name: "TARGETOS", Value: targetOS},
		{Name: "TARGETARCH", Value: targetArch},
		{Name: "TARGETVARIANT", Value: targetVariant},

		{Name: "BUILDPLATFORM", Value: targetPlatform},
		{Name: "BUILDOS", Value: targetOS},
		{Name: "BUILDARCH", Value: targetArch},
		{Name: "BUILDVARIANT", Value: targetVariant},

		{Name: "EXECUTOR_PLATFORM", Value: "k8s"},

		{Name: "STORAGE_ENDPOINT", Value: os.Getenv("S3_ENDPOINT")},
		{Name: "STORAGE_REGION", Value: os.Getenv("S3_REGION")},
		{Name: "STORAGE_USE_SSL", Value: os.Getenv("S3_SSL")},
		{Name: "STORAGE_ACCESS_KEY", Value: os.Getenv("S3_ACCESS_KEY")},
		{Name: "STORAGE_SECRET_KEY", Value: os.Getenv("S3_SECRET_KEY")},

		{Name: "CONTEXT_BUCKET", Value: contextBucket},
		{Name: "CONTEXT_KEY", Value: contextKey},

		{Name: "CONTROLLER_URL", Value: k.ControllerURL},
		{Name: "INGEST_URL", Value: ingestURL},
	}

	var kanikoDestination string
	if st.IsSingleArch {
		if ef.Destination != "" {
			kanikoDestination = ef.Destination
		} else {
			kanikoDestination = st.GlobalDestination
		}
	} else {
		if ef.Destination != "" && ef.Destination != st.GlobalDestination {
			kanikoDestination = ef.Destination
		} else {
			if st.HasDuplicateArch {
				kanikoDestination = appendTaskSuffix(st.GlobalDestination, taskID)
			} else {
				kanikoDestination = appendArchSuffix(st.GlobalDestination, arch)
			}
		}
	}

	envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_DESTINATION", Value: kanikoDestination})
	envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CONTEXT", Value: ef.ContextPath})
	envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_DOCKERFILE", Value: ef.Dockerfile})

	if len(ef.BuildArgs) > 0 {
		var pairs []string
		for k, v := range ef.BuildArgs {
			pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
		}
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_BUILD_ARGS", Value: strings.Join(pairs, ",")})
	}

	if len(ef.KanikoCredentials) > 0 {
		creds, err := createDockerConfigJSON(ef.KanikoCredentials)
		if err != nil {
			return fmt.Errorf("create docker config: %w", err)
		}
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CREDENTIALS_JSON", Value: creds})
	}

	if ef.CacheEnable != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CACHE_ENABLE", Value: fmt.Sprintf("%t", *ef.CacheEnable)})
	}
	if ef.CacheRepo != "" {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CACHE_REPO", Value: ef.CacheRepo})
	}
	if ef.CacheTTL != "" {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CACHE_TTL", Value: ef.CacheTTL})
	}
	if ef.CacheCopyLayers != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CACHE_COPY_LAYERS", Value: fmt.Sprintf("%t", *ef.CacheCopyLayers)})
	}
	if ef.CacheRunLayers != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CACHE_RUN_LAYERS", Value: fmt.Sprintf("%t", *ef.CacheRunLayers)})
	}
	if ef.CacheCompressed != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CACHE_COMPRESSED", Value: fmt.Sprintf("%t", *ef.CacheCompressed)})
	}

	if ef.SnapshotMode != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_SNAPSHOT_MODE", Value: *ef.SnapshotMode})
	}
	if ef.UseNewRun != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_USE_NEW_RUN", Value: fmt.Sprintf("%t", *ef.UseNewRun)})
	}
	if ef.Cleanup != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CLEANUP", Value: fmt.Sprintf("%t", *ef.Cleanup)})
	}
	if ef.CustomPlatform != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_CUSTOM_PLATFORM", Value: *ef.CustomPlatform})
	}
	if ef.NoPush != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_NO_PUSH", Value: fmt.Sprintf("%t", *ef.NoPush)})
	}

	if len(ef.IgnorePath) > 0 {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_IGNORE_PATH", Value: strings.Join(ef.IgnorePath, ",")})
	}

	if ef.ExtraFlags != "" {
		envVars = append(envVars, apiv1.EnvVar{Name: "KANIKO_EXTRA_FLAGS", Value: ef.ExtraFlags})
	}

	if ef.PreScript != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "PRE_SCRIPT", Value: *ef.PreScript})
	}
	if ef.PostScript != nil {
		envVars = append(envVars, apiv1.EnvVar{Name: "POST_SCRIPT", Value: *ef.PostScript})
	}

	for key, value := range ef.Env {
		envVars = append(envVars, apiv1.EnvVar{Name: key, Value: value})
	}

	resourceLimits := apiv1.ResourceList{}

	if ef.CPU != "" {
		cpuFormatted := config.FormatK8sResource(ef.CPU, "cpu")
		q, err := resource.ParseQuantity(cpuFormatted)
		if err != nil {
			return fmt.Errorf("invalid cpu=%s (formatted=%s): %w", ef.CPU, cpuFormatted, err)
		}
		resourceLimits[apiv1.ResourceCPU] = q
		st.AppendLog("info", fmt.Sprintf("[k8s][%s] cpu limit: %s", taskID, cpuFormatted))
	}

	if ef.Memory != "" {
		memFormatted := config.FormatK8sResource(ef.Memory, "memory")
		q, err := resource.ParseQuantity(memFormatted)
		if err != nil {
			return fmt.Errorf("invalid memory=%s (formatted=%s): %w", ef.Memory, memFormatted, err)
		}
		resourceLimits[apiv1.ResourceMemory] = q
		st.AppendLog("info", fmt.Sprintf("[k8s][%s] memory limit: %s", taskID, memFormatted))
	}

	var nodeSelector map[string]string
	var tolerations []apiv1.Toleration
	var imagePullSecrets []apiv1.LocalObjectReference
	serviceAccount := "default"

	podSpec := apiv1.PodSpec{
		RestartPolicy:      apiv1.RestartPolicyNever,
		ServiceAccountName: serviceAccount,
		NodeSelector:       nodeSelector,
		Tolerations:        tolerations,
		ImagePullSecrets:   imagePullSecrets,

		Containers: []apiv1.Container{
			{
				Name:  "agent",
				Image: k.AgentImage,
				Env:   envVars,
				Resources: apiv1.ResourceRequirements{
					Limits: resourceLimits,
				},
			},
		},
	}

	k.applyServerPodSpec(&podSpec, arch)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: jobName,
			Namespace:    k.Namespace,
			Labels: map[string]string{
				"build-id": st.ID,
				"task-id":  taskID,
				"arch":     arch,
			},
		},
		Spec: batchv1.JobSpec{
			Template: apiv1.PodTemplateSpec{
				Spec: podSpec,
			},
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(1800),
		},
	}

	created, err := k.Client.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("[k8s] create job: %w", err)
	}

	jobName = created.Name

	st.Mu.Lock()
	st.TaskArnByID[taskID] = jobName
	st.IDByTaskArn[jobName] = taskID
	st.Mu.Unlock()

	st.AppendLog("info", fmt.Sprintf("[k8s][%s] started job: %s", taskID, jobName))

	done := make(chan struct{})
	watchCtx, watchCancel := context.WithTimeout(ctx, 30*time.Minute)
	defer watchCancel()

	go func() {
		defer close(done)
		k.waitJobCompletion(watchCtx, st, taskID, jobName)
	}()

	select {
	case <-done:
		if st.HasError() {
			return st.GetError()
		}
		return nil

	case <-ctx.Done():
		return fmt.Errorf("k8s job wait cancelled: %w", ctx.Err())
	}
}

func (k *K8sExecutor) waitJobCompletion(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	jobName string,
) {
	watcher, err := k.Client.BatchV1().Jobs(k.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", jobName),
	})
	if err != nil {
		st.AppendLog("error", fmt.Sprintf("[k8s][%s] watch error: %v", taskID, err))
		st.SetError(err)
		return
	}
	defer watcher.Stop()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			st.AppendLog("error", fmt.Sprintf("[k8s][%s] context cancelled: %v", taskID, ctx.Err()))
			st.SetError(fmt.Errorf("job timeout: %w", ctx.Err()))
			k.checkPodExitCode(context.Background(), st, taskID, jobName, ctx.Err())
			return

		case event, ok := <-watcher.ResultChan():
			if !ok {
				k.checkJobStatus(ctx, st, taskID, jobName)
				return
			}

			if event.Type == watch.Modified || event.Type == watch.Deleted {
				job, ok := event.Object.(*batchv1.Job)
				if !ok {
					continue
				}

				for _, cond := range job.Status.Conditions {
					if cond.Type == batchv1.JobComplete && cond.Status == apiv1.ConditionTrue {
						k.checkPodExitCode(context.Background(), st, taskID, jobName, nil)
						return
					}

					if cond.Type == batchv1.JobFailed && cond.Status == apiv1.ConditionTrue {
						k.checkPodExitCode(context.Background(), st, taskID, jobName, fmt.Errorf("job failed: %s", cond.Reason))
						return
					}
				}
			}

		case <-ticker.C:
			job, err := k.Client.BatchV1().Jobs(k.Namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				continue
			}

			st.AppendLog("debug", fmt.Sprintf("[k8s][%s] active=%d succeeded=%d failed=%d",
				taskID, job.Status.Active, job.Status.Succeeded, job.Status.Failed))
		}
	}
}

// getTaskColorIndex 는 taskID에 해당하는 색상 인덱스를 문자열로 반환합니다.
// amd64 계열은 짝수, arm64 계열은 홀수 인덱스를 사용합니다.
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

func (k *K8sExecutor) checkJobStatus(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	jobName string,
) {
	job, err := k.Client.BatchV1().Jobs(k.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		st.SetError(err)
		k.checkPodExitCode(ctx, st, taskID, jobName, err)
		return
	}

	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == apiv1.ConditionTrue {
			k.checkPodExitCode(ctx, st, taskID, jobName, nil)
			return
		}

		if cond.Type == batchv1.JobFailed && cond.Status == apiv1.ConditionTrue {
			k.checkPodExitCode(ctx, st, taskID, jobName, fmt.Errorf("job failed: %s", cond.Reason))
			return
		}
	}

	k.checkPodExitCode(ctx, st, taskID, jobName, fmt.Errorf("job status unclear"))
}

func (k *K8sExecutor) checkPodExitCode(
	ctx context.Context,
	st *state.BuildState,
	taskID string,
	jobName string,
	jobErr error,
) {
	pods, err := k.Client.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})

	if err != nil {
		st.AppendLog("error", fmt.Sprintf("[k8s][%s] failed to list pods: %v", taskID, err))
		st.SetError(err)
		return
	}

	if len(pods.Items) == 0 {
		err := fmt.Errorf("no pods found for job %s", jobName)
		st.AppendLog("error", fmt.Sprintf("[k8s][%s] %v", taskID, err))
		st.SetError(err)
		return
	}

	pod := pods.Items[0]

	if pod.Status.Phase == apiv1.PodPending || pod.Status.Phase == apiv1.PodUnknown {
		err := fmt.Errorf("pod never started: phase=%s", pod.Status.Phase)
		st.AppendLog("error", fmt.Sprintf("[k8s][%s] %v", taskID, err))
		st.SetError(err)
		return
	}

	foundAgent := false
	var taskErr error

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" {
			foundAgent = true

			if cs.State.Terminated != nil {
				exitCode := cs.State.Terminated.ExitCode

				if exitCode != 0 {
					taskErr = fmt.Errorf("agent exit=%d: %s", exitCode, cs.State.Terminated.Reason)
					st.AppendLog("error", fmt.Sprintf("[k8s][%s] %v", taskID, taskErr))
					st.SetError(taskErr)
				} else {
					st.AppendLog("info", fmt.Sprintf("[k8s][%s] exit=0 success", taskID))
				}

				maxResultWait := 30 * time.Second
				maxIngestWait := 90 * time.Second
				interval := 100 * time.Millisecond

				resultWaited := time.Duration(0)
				for resultWaited < maxResultWait {
					st.Mu.RLock()
					_, hasResult := st.Results[taskID]
					st.Mu.RUnlock()

					if hasResult {
						st.AppendLog("debug", fmt.Sprintf("[k8s][%s] result received", taskID))
						break
					}

					if resultWaited%(5*time.Second) == 0 && resultWaited > 0 {
						st.AppendLog("debug", fmt.Sprintf("[k8s][%s] waiting for result... (%v elapsed)",
							taskID, resultWaited))
					}

					time.Sleep(interval)
					resultWaited += interval
				}

				st.Mu.RLock()
				_, hasResult := st.Results[taskID]
				st.Mu.RUnlock()

				if !hasResult {
					st.AppendLog("warn", fmt.Sprintf("[k8s][%s] result not received after %v",
						taskID, maxResultWait))
				}

				ingestWaited := time.Duration(0)
				for ingestWaited < maxIngestWait {
					st.Mu.RLock()
					ingestDone := st.IngestDone[taskID]
					st.Mu.RUnlock()

					if ingestDone {
						st.AppendLog("debug", fmt.Sprintf("[k8s][%s] ingest completed", taskID))
						break
					}

					if ingestWaited%(5*time.Second) == 0 && ingestWaited > 0 {
						st.AppendLog("debug", fmt.Sprintf("[k8s][%s] waiting for ingest completion... (%v elapsed)",
							taskID, ingestWaited))
					}

					time.Sleep(interval)
					ingestWaited += interval
				}

				st.Mu.RLock()
				ingestDone := st.IngestDone[taskID]
				st.Mu.RUnlock()

				if !ingestDone {
					st.AppendLog("warn", fmt.Sprintf("[k8s][%s] ingest not confirmed after %v (may already be closed)",
						taskID, maxIngestWait))
				}

				st.MarkIngestDone(taskID)

				return
			}

			st.AppendLog("warn", fmt.Sprintf("[k8s][%s] agent container not terminated yet: %+v",
				taskID, cs.State))
		}
	}

	if !foundAgent {
		err := fmt.Errorf("agent container not found in pod")
		st.AppendLog("error", fmt.Sprintf("[k8s][%s] %v", taskID, err))
		st.SetError(err)
		return
	}

	if jobErr != nil {
		st.AppendLog("error", fmt.Sprintf("[k8s][%s] job error: %v", taskID, jobErr))
		st.SetError(jobErr)
	}
}

func int32Ptr(v int32) *int32 { return &v }

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

func createDockerConfigJSON(creds []config.RegistryCredential) (string, error) {
	type DockerAuth struct {
		Auth string `json:"auth"`
	}
	type DockerConfig struct {
		Auths map[string]DockerAuth `json:"auths"`
	}

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

func (k *K8sExecutor) applyServerPodSpec(podSpec *apiv1.PodSpec, arch string) {
	serviceAccount := "default"

	if k.K8sConfig == nil {
		podSpec.ServiceAccountName = serviceAccount
		if podSpec.NodeSelector == nil {
			podSpec.NodeSelector = map[string]string{}
		}
		if _, ok := podSpec.NodeSelector["kubernetes.io/arch"]; !ok {
			podSpec.NodeSelector["kubernetes.io/arch"] = arch
		}
		return
	}

	cfg := k.K8sConfig

	if cfg.ServiceAccountName != nil && strings.TrimSpace(*cfg.ServiceAccountName) != "" {
		serviceAccount = strings.TrimSpace(*cfg.ServiceAccountName)
	}
	podSpec.ServiceAccountName = serviceAccount

	if len(cfg.NodeSelector) > 0 {
		ns := make(map[string]string, len(cfg.NodeSelector)+1)
		for kk, vv := range cfg.NodeSelector {
			ns[kk] = vv
		}
		podSpec.NodeSelector = ns
	}

	if podSpec.NodeSelector == nil {
		podSpec.NodeSelector = map[string]string{}
	}
	if _, ok := podSpec.NodeSelector["kubernetes.io/arch"]; !ok {
		podSpec.NodeSelector["kubernetes.io/arch"] = arch
	}

	if len(cfg.Tolerations) > 0 {
		ts := make([]apiv1.Toleration, 0, len(cfg.Tolerations))
		for _, t := range cfg.Tolerations {
			tol := apiv1.Toleration{
				Key:      t.Key,
				Value:    t.Value,
				Effect:   apiv1.TaintEffect(t.Effect),
				Operator: apiv1.TolerationOperator(t.Operator),
			}
			if strings.TrimSpace(string(tol.Operator)) == "" {
				tol.Operator = apiv1.TolerationOpExists
			}
			ts = append(ts, tol)
		}
		podSpec.Tolerations = ts
	}

	if len(cfg.ImagePullSecrets) > 0 {
		ips := make([]apiv1.LocalObjectReference, 0, len(cfg.ImagePullSecrets))
		for _, s := range cfg.ImagePullSecrets {
			if strings.TrimSpace(s.Name) == "" {
				continue
			}
			ips = append(ips, apiv1.LocalObjectReference{Name: strings.TrimSpace(s.Name)})
		}
		if len(ips) > 0 {
			podSpec.ImagePullSecrets = ips
		}
	}
}
