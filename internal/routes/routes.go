package routes

import (
	"bufio"
	"github.com/rayshoo/bakery/internal/orchestrator"
	"github.com/rayshoo/bakery/internal/state"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

type Dependencies struct {
	Orch  *orchestrator.Orchestrator
	Store *state.Store
}

type AgentResult struct {
	TaskID      string `json:"taskId"`
	Arch        string `json:"arch"`
	ImageDigest string `json:"imageDigest"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

// Setup 은 Fiber 앱에 빌드 관련 라우트를 등록합니다.
func Setup(app *fiber.App, deps Dependencies) {

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("build controller is running")
	})

	app.Post("/build", func(c *fiber.Ctx) error {
		body := c.Body()
		if len(body) == 0 {
			return fiber.NewError(400, "empty body")
		}

		contextKey := c.Query("context_key")
		if contextKey == "" {
			return fiber.NewError(400, "missing context_key")
		}

		contextBucket := os.Getenv("S3_BUCKET")
		if contextBucket == "" {
			return fiber.NewError(500, "S3_BUCKET not configured")
		}

		serviceName := c.Query("service_name", "")

		buildID, _, err := deps.Orch.StartBuild(body, contextBucket, contextKey, serviceName)
		if err != nil {
			return fiber.NewError(500, err.Error())
		}

		return c.JSON(fiber.Map{
			"buildID": buildID,
			"status":  "started",
		})
	})

	app.Get("/build/:id/logs", func(c *fiber.Ctx) error {
		buildID := string([]byte(c.Params("id")))

		st, ok := deps.Store.Get(buildID)
		if !ok {
			return fiber.NewError(fiber.StatusNotFound, "unknown build id")
		}

		c.Set("Content-Type", "application/json")
		c.Set("Transfer-Encoding", "chunked")
		c.Set("X-Content-Type-Options", "nosniff")

		c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
			for {
				select {
				case logEntry, ok := <-st.Logs:
					if !ok {
						st.Mu.RLock()
						finalErr := st.GetError()
						st.Mu.RUnlock()

						var finalMsg state.LogEntry
						if finalErr != nil {
							finalMsg = state.LogEntry{
								TS:      time.Now(),
								Level:   "error",
								Message: "BUILD FAILED",
							}
						} else {
							finalMsg = state.LogEntry{
								TS:      time.Now(),
								Level:   "info",
								Message: "BUILD SUCCEEDED",
							}
						}
						_ = writeJSON(w, finalMsg)
						return
					}
					_ = writeJSON(w, logEntry)

				case <-st.Done:
				}
			}
		})

		return nil
	})

	app.Post("/build/:id/logs/ingest", func(c *fiber.Ctx) error {
		buildID := string([]byte(c.Params("id")))
		st, ok := deps.Store.Get(buildID)
		if !ok {
			return fiber.NewError(404, "unknown build id")
		}

		if st.ID != buildID {
			return fiber.NewError(500, fmt.Sprintf("state ID mismatch: expected %s, got %s", buildID, st.ID))
		}

		taskID := string([]byte(c.Query("task")))
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			taskID = "unknown"
		}
		st.AppendLog("debug", fmt.Sprintf("ingest from task=%s", taskID))

		stream := c.Context().RequestBodyStream()
		var reader *bufio.Reader

		if stream != nil {
			reader = bufio.NewReader(stream)
		} else {
			body := c.Body()
			if len(body) == 0 {
				st.AppendLog("debug", fmt.Sprintf("ingest closed for task=%s (empty body)", taskID))
				st.MarkIngestDone(taskID)
				return c.SendStatus(200)
			}
			reader = bufio.NewReader(bytes.NewReader(body))
		}

		for {
			line, err := reader.ReadString('\n')

			if len(line) > 0 {
				st.MarkIngestStarted(taskID)
				st.AppendLog("info", strings.TrimRight(line, "\r\n"))
			}

			if err != nil {
				st.AppendLog("debug", fmt.Sprintf("ingest closed for task=%s (EOF)", taskID))
				st.MarkIngestDone(taskID)
				break
			}
		}

		return c.SendStatus(200)
	})

	app.Post("/build/:id/result", func(c *fiber.Ctx) error {
		buildID := string([]byte(c.Params("id")))
		queryTaskID := string([]byte(c.Query("task")))
		bodyBytes := make([]byte, len(c.Body()))
		copy(bodyBytes, c.Body())

		var result AgentResult
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			return fiber.NewError(400, "invalid json")
		}

		taskID := strings.TrimSpace(queryTaskID)
		if taskID == "" {
			taskID = strings.TrimSpace(result.TaskID)
		}
		if taskID == "" {
			return fiber.NewError(400, "missing task parameter")
		}

		st, ok := deps.Store.Get(buildID)
		if !ok {
			return fiber.NewError(404, "unknown build id")
		}

		if st.ID != buildID {
			return fiber.NewError(500, fmt.Sprintf("state ID mismatch: expected %s, got %s", buildID, st.ID))
		}

		st.AppendLog("debug", fmt.Sprintf("[result] Received: buildID=%s, query_task=%s, body_taskID=%s, final_taskID=%s, arch=%s",
			buildID, queryTaskID, result.TaskID, taskID, result.Arch))

		st.Mu.Lock()

		beforeKeys := make([]string, 0, len(st.Results))
		for k := range st.Results {
			beforeKeys = append(beforeKeys, k)
		}
		beforeCount := st.ResultsReceived

		if _, exists := st.Results[taskID]; exists {
			existingResult := st.Results[taskID]

			var logMsg string
			if existingResult.ImageDigest == result.ImageDigest {
				logMsg = fmt.Sprintf("[result] Duplicate result for task '%s' with same digest - ignoring", taskID)
			} else {
				logMsg = fmt.Sprintf("[result] CRITICAL: Duplicate result for task '%s' with DIFFERENT digest! (existing=%s, new=%s) - REJECTING NEW",
					taskID, existingResult.ImageDigest, result.ImageDigest)
			}

			st.Mu.Unlock()

			if existingResult.ImageDigest == result.ImageDigest {
				st.AppendLog("debug", logMsg)
			} else {
				st.AppendLog("error", logMsg)
			}
			return c.SendStatus(200)
		}

		st.Results[taskID] = state.TaskResult{
			Arch:        result.Arch,
			ImageDigest: result.ImageDigest,
			Success:     result.Success,
			Error:       result.Error,
		}
		st.ResultsReceived++

		if !result.Success && st.FirstError == nil {
			st.FirstError = fmt.Errorf("task %s failed: %s", taskID, result.Error)
		}

		afterKeys := make([]string, 0, len(st.Results))
		for k := range st.Results {
			afterKeys = append(afterKeys, k)
		}
		afterCount := st.ResultsReceived
		stateID := st.ID
		digestShort := result.ImageDigest
		if len(digestShort) > 12 {
			digestShort = digestShort[:12]
		}

		st.Mu.Unlock()

		st.AppendLog("info", fmt.Sprintf("[result] Saved: stateID=%s, taskID='%s', arch=%s, digest=%s, before=%v(%d), after=%v(%d)",
			stateID, taskID, result.Arch, digestShort, beforeKeys, beforeCount, afterKeys, afterCount))

		return c.SendStatus(200)
	})
}

func writeJSON(w *bufio.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n"))
	_ = w.Flush()
	return nil
}
