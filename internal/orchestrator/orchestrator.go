package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rayshoo/bakery/internal/config"
	"github.com/rayshoo/bakery/internal/ecs"
	"github.com/rayshoo/bakery/internal/registry"
	"github.com/rayshoo/bakery/internal/state"

	"github.com/google/uuid"
)

// Executor is the interface for running build tasks.
type Executor interface {
	RunTask(
		ctx context.Context,
		st *state.BuildState,
		taskID string,
		ef config.EffectiveConfig,
		contextBucket string,
		contextKey string,
		ingestURL string,
	) error
}

type Deps struct {
	Store         *state.Store
	ECS           Executor
	K8S           Executor
	ControllerURL string
	S3Endpoint    string
	S3Bucket      string
	S3Region      string
	S3PathStyle   bool
}

// Orchestrator distributes build tasks across executors and collects results.
type Orchestrator struct {
	store         *state.Store
	ecs           Executor
	k8s           Executor
	controllerURL string

	S3Endpoint  string
	S3Bucket    string
	S3Region    string
	S3PathStyle bool
}

func New(d Deps) *Orchestrator {
	return &Orchestrator{
		store:         d.Store,
		ecs:           d.ECS,
		k8s:           d.K8S,
		controllerURL: d.ControllerURL,
		S3Endpoint:    d.S3Endpoint,
		S3Bucket:      d.S3Bucket,
		S3Region:      d.S3Region,
		S3PathStyle:   d.S3PathStyle,
	}
}

// StartBuild accepts a build request, starts tasks, and returns a BuildState.
func (o *Orchestrator) StartBuild(
	yamlBytes []byte,
	contextBucket string,
	contextKey string,
	serviceName string,
) (string, *state.BuildState, error) {

	var cfg config.BuildConfig
	if err := config.UnmarshalYAML(yamlBytes, &cfg); err != nil {
		return "", nil, fmt.Errorf("parse yaml: %w", err)
	}

	effectiveList, err := config.BuildEffectiveList(&cfg)
	if err != nil {
		return "", nil, fmt.Errorf("invalid yaml config: %w", err)
	}

	var pushTasks []config.EffectiveConfig
	for _, ef := range effectiveList {
		if ef.NoPush == nil || !*ef.NoPush {
			pushTasks = append(pushTasks, ef)
		}
	}

	taskCount := len(effectiveList)
	buildID := generateBuildID(serviceName)

	archCount := make(map[string]int)
	for _, ef := range pushTasks {
		archCount[ef.Arch]++
	}

	hasDuplicateArch := false
	for _, count := range archCount {
		if count > 1 {
			hasDuplicateArch = true
			break
		}
	}

	isSingleArch := len(pushTasks) <= 1
	globalDestination := cfg.Global.Kaniko.Destination

	st := state.NewBuildState(buildID, taskCount, isSingleArch, globalDestination)
	st.HasDuplicateArch = hasDuplicateArch
	o.store.Register(buildID, st)

	st.AppendLog("info", "build accepted by orchestrator")
	st.AppendLog("info", fmt.Sprintf("%d build tasks found", taskCount))

	ingestURL := fmt.Sprintf("%s/build/%s/logs/ingest", o.controllerURL, buildID)
	var wg sync.WaitGroup

	for idx, ef := range effectiveList {
		wg.Add(1)

		var taskID string
		if hasDuplicateArch {
			taskID = fmt.Sprintf("%s-%d", ef.Arch, idx)
		} else {
			taskID = ef.Arch
		}

		go func(i int, cfg config.EffectiveConfig, tid string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					err := fmt.Errorf("panic in task %s: %v", tid, r)
					st.AppendLog("error", err.Error())
					st.SetError(err)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), getenvDuration("BUILD_TASK_TIMEOUT", 30*time.Minute))
			defer cancel()

			st.AppendLog("info", fmt.Sprintf("[task %s] starting (%s / %s)", tid, cfg.Platform, cfg.Arch))

			var execErr error
			switch cfg.Platform {
			case "ecs":
				ecsExec, ok := o.ecs.(*ecs.ECSExecutor)
				if !ok {
					execErr = fmt.Errorf("ECS executor type mismatch")
				} else {
					execErr = ecsExec.RunTaskForArch(
						ctx, st, tid, cfg,
						contextBucket, contextKey,
						ingestURL,
						isSingleArch,
						globalDestination,
					)
				}
			case "k8s":
				if o.k8s == nil {
					execErr = fmt.Errorf("K8s executor not configured")
				} else {
					execErr = o.k8s.RunTask(ctx, st, tid, cfg, contextBucket, contextKey, ingestURL)
				}
			default:
				execErr = fmt.Errorf("unknown platform: %s", cfg.Platform)
			}

			if execErr != nil {
				st.AppendLog("error", fmt.Sprintf("[task %s] failed: %v", tid, execErr))
				st.SetError(execErr)
			} else {
				st.AppendLog("info", fmt.Sprintf("[task %s] executor finished", tid))
			}
		}(idx, ef, taskID)
	}

	go func() {
		wg.Wait()

		st.Mu.RLock()
		currentKeys := make([]string, 0, len(st.Results))
		for k := range st.Results {
			currentKeys = append(currentKeys, k)
		}
		currentReceived := st.ResultsReceived
		st.Mu.RUnlock()

		st.AppendLog("debug", fmt.Sprintf("all executors finished. stateID=%s, results: %d/%d, keys=%v",
			st.ID, currentReceived, st.TotalTasks, currentKeys))

		maxWait := getenvDuration("BUILD_RESULT_TIMEOUT", 1*time.Minute)
		startWait := time.Now()

		for {
			if st.AllResultsReceived() {
				break
			}
			if time.Since(startWait) > maxWait {
				break
			}

			if int(time.Since(startWait).Seconds())%5 == 0 {
				st.Mu.RLock()
				receivedKeys := []string{}
				for k := range st.Results {
					receivedKeys = append(receivedKeys, k)
				}
				st.Mu.RUnlock()
				st.AppendLog("debug", fmt.Sprintf("waiting for results... received: %v", receivedKeys))
			}

			time.Sleep(1 * time.Second)
		}

		if !st.AllResultsReceived() {
			st.Mu.RLock()
			err := fmt.Errorf("timeout waiting for agent results (%d/%d received)", st.ResultsReceived, st.TotalTasks)
			st.Mu.RUnlock()
			st.AppendLog("error", err.Error())
			st.SetError(err)
		}

		if !isSingleArch && !st.HasError() {
			st.AppendLog("info", "starting multi-arch manifest creation")
			ctx := context.Background()
			if err := o.createManifest(ctx, st, globalDestination, effectiveList); err != nil {
				st.AppendLog("error", fmt.Sprintf("manifest creation failed: %v", err))
				st.SetError(err)
			} else {
				st.AppendLog("info", fmt.Sprintf("multi-arch manifest created: %s", globalDestination))
			}
		}

		st.Finish(st.GetError())
	}()

	return buildID, st, nil
}

func (o *Orchestrator) createManifest(
	ctx context.Context,
	st *state.BuildState,
	destination string,
	allTasks []config.EffectiveConfig,
) error {
	var images []registry.PlatformImage

	st.Mu.RLock()
	actualKeys := make([]string, 0, len(st.Results))
	resultDetails := make([]string, 0, len(st.Results))
	for k, v := range st.Results {
		actualKeys = append(actualKeys, k)
		digestShort := v.ImageDigest
		if len(digestShort) > 12 {
			digestShort = digestShort[:12]
		}
		resultDetails = append(resultDetails, fmt.Sprintf("%s->%s(%s)", k, v.Arch, digestShort))
	}
	buildID := st.ID
	totalTasks := st.TotalTasks
	resultsReceived := st.ResultsReceived
	mapLen := len(st.Results)
	st.Mu.RUnlock()

	st.AppendLog("debug", fmt.Sprintf("createManifest: buildID=%s, Tasks=%d, ResultsReceived=%d, mapLen=%d, Keys=%v, Details=%v",
		buildID, totalTasks, resultsReceived, mapLen, actualKeys, resultDetails))

	for idx, ef := range allTasks {
		if ef.NoPush != nil && *ef.NoPush {
			continue
		}

		var taskID string
		if st.HasDuplicateArch {
			taskID = fmt.Sprintf("%s-%d", ef.Arch, idx)
		} else {
			taskID = ef.Arch
		}

		st.AppendLog("debug", fmt.Sprintf("Looking for result with taskID='%s' (arch=%s, idx=%d, hasDuplicate=%v)",
			taskID, ef.Arch, idx, st.HasDuplicateArch))

		st.Mu.RLock()
		result, ok := st.Results[taskID]
		st.Mu.RUnlock()

		if !ok {
			return fmt.Errorf("missing result for task '%s' (buildID=%s, arch=%s, idx=%d). Available keys: %v. Total expected: %d, Received: %d",
				taskID, buildID, ef.Arch, idx, actualKeys, totalTasks, resultsReceived)
		}

		if !result.Success {
			return fmt.Errorf("task %s build failed: %s", taskID, result.Error)
		}

		var pushedImage string
		if ef.Destination != "" {
			pushedImage = ef.Destination
		} else {
			if st.HasDuplicateArch {
				pushedImage = appendTaskSuffix(destination, taskID)
			} else {
				pushedImage = appendArchSuffix(destination, ef.Arch)
			}
		}

		st.AppendLog("debug", fmt.Sprintf("Adding to manifest: taskID=%s, image=%s, digest=%s",
			taskID, pushedImage, result.ImageDigest))

		images = append(images, registry.PlatformImage{
			Arch:   ef.Arch,
			Image:  pushedImage,
			Digest: result.ImageDigest,
		})
	}

	st.AppendLog("info", fmt.Sprintf("Creating multi-arch manifest with %d images", len(images)))
	return registry.CreateManifestList(ctx, st, images, destination)
}

func appendArchSuffix(destination, arch string) string {
	if idx := lastIndexByte(destination, ':'); idx != -1 {
		return fmt.Sprintf("%s:%s_%s", destination[:idx], destination[idx+1:], arch)
	}
	return fmt.Sprintf("%s:latest_%s", destination, arch)
}

func appendTaskSuffix(destination, taskID string) string {
	if idx := lastIndexByte(destination, ':'); idx != -1 {
		return fmt.Sprintf("%s:%s_%s", destination[:idx], destination[idx+1:], taskID)
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

func generateBuildID(serviceName string) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	randomHex := hex.EncodeToString(b)

	ts := time.Now().UnixNano()
	if serviceName != "" {
		return fmt.Sprintf("b-%d-%s-%s", ts, randomHex, serviceName)
	}
	return fmt.Sprintf("b-%d-%s", ts, uuid.New().String()[:8])
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
