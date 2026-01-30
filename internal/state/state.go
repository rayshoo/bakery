package state

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func debugLog(format string, v ...interface{}) {
	if os.Getenv("SERVER_LOG_LEVEL") == "debug" {
		log.Printf(format, v...)
	}
}

type LogEntry struct {
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type TaskResult struct {
	Arch        string
	ImageDigest string
	Success     bool
	Error       string
}

// BuildState 는 단일 빌드의 상태를 관리하는 구조체입니다.
// ID 필드는 생성 후 변경되지 않으며, 로그 스트리밍과 결과 수집에 사용됩니다.
type BuildState struct {
	ID     string
	Logs   chan LogEntry
	Done   chan struct{}
	Mu     sync.RWMutex
	closed bool

	TaskArnByID   map[string]string
	IDByTaskArn   map[string]string
	IngestStarted map[string]bool
	IngestDone    map[string]bool
	TotalTasks    int

	IngestDoneCt int
	finished     bool
	FirstError   error

	Results         map[string]TaskResult
	ResultsReceived int

	IsSingleArch      bool
	GlobalDestination string
	HasDuplicateArch  bool
}

// Store 는 빌드 상태들을 관리하는 저장소입니다.
type Store struct {
	mu     sync.RWMutex
	states map[string]*BuildState
}

func NewStore() *Store {
	return &Store{
		states: make(map[string]*BuildState),
	}
}

func (s *Store) Register(id string, st *BuildState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if st.ID != id {
		log.Printf("[Store.Register] CRITICAL: ID mismatch! param=%s, state.ID=%s", id, st.ID)
	}

	if existing, exists := s.states[id]; exists {
		debugLog("[Store.Register] WARNING: Overwriting existing state for id=%s (old=%p, new=%p)",
			id, existing, st)
	}

	s.states[id] = st
	debugLog("[Store.Register] id=%s, state=%p, total_states=%d", id, st, len(s.states))
}

func (s *Store) Get(id string) (*BuildState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, ok := s.states[id]
	if !ok {
		keys := make([]string, 0, len(s.states))
		for k := range s.states {
			keys = append(keys, k)
		}
		debugLog("[Store.Get] id=%s → NOT FOUND (registered: %v)", id, keys)
		return nil, false
	}

	if st.ID != id {
		log.Printf("[Store.Get] CRITICAL BUG: requested id=%s but state.ID=%s - returning nil", id, st.ID)
		return nil, false
	}

	debugLog("[Store.Get] id=%s → found state=%p", id, st)
	return st, ok
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, id)
	debugLog("[Store.Delete] id=%s, remaining=%d", id, len(s.states))
}

func (s *Store) ListIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make([]string, 0, len(s.states))
	for k := range s.states {
		ids = append(ids, k)
	}
	return ids
}

// NewBuildState 는 새로운 빌드 상태를 생성합니다.
func NewBuildState(id string, totalTasks int, isSingleArch bool, globalDest string) *BuildState {
	if strings.TrimSpace(id) == "" {
		panic("NewBuildState: ID cannot be empty")
	}

	st := &BuildState{
		ID:                id,
		Logs:              make(chan LogEntry, 1000),
		Done:              make(chan struct{}),
		TaskArnByID:       make(map[string]string),
		IDByTaskArn:       make(map[string]string),
		IngestStarted:     make(map[string]bool),
		IngestDone:        make(map[string]bool),
		TotalTasks:        totalTasks,
		Results:           make(map[string]TaskResult),
		IsSingleArch:      isSingleArch,
		GlobalDestination: globalDest,
		HasDuplicateArch:  false,
	}

	debugLog("[NewBuildState] Created: id=%s, totalTasks=%d", id, totalTasks)
	return st
}

func (s *BuildState) AppendLog(level, msg string) {
	s.appendLog(level, msg, false)
}

func (s *BuildState) appendLog(level, msg string, fromFinish bool) {
	entry := LogEntry{
		TS:      time.Now(),
		Level:   level,
		Message: msg,
	}

	s.Mu.RLock()
	if !fromFinish && s.finished {
		s.Mu.RUnlock()
		return
	}
	ch := s.Logs
	s.Mu.RUnlock()

	defer func() { recover() }()

	select {
	case ch <- entry:
	default:
	}
}

func (s *BuildState) MarkIngestStarted(taskID string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.IngestStarted[taskID] = true
}

func (s *BuildState) MarkIngestDone(taskID string) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	if s.IngestDone[taskID] {
		return false
	}

	s.IngestDone[taskID] = true
	s.IngestDoneCt++

	return s.IngestDoneCt == s.TotalTasks
}

func (s *BuildState) SetResult(taskID, arch, digest string, success bool, errMsg string) {
	taskID = strings.TrimSpace(taskID)

	s.Mu.Lock()
	defer s.Mu.Unlock()

	if existing, exists := s.Results[taskID]; exists {
		debugLog("[SetResult] WARNING: state=%s overwriting taskID='%s' (old_arch=%s, new_arch=%s)",
			s.ID, taskID, existing.Arch, arch)
	}

	s.Results[taskID] = TaskResult{
		Arch:        arch,
		ImageDigest: digest,
		Success:     success,
		Error:       errMsg,
	}
	s.ResultsReceived++

	if !success && s.FirstError == nil {
		s.FirstError = fmt.Errorf("task %s failed: %s", taskID, errMsg)
	}

	debugLog("[SetResult] state=%s, taskID='%s', count=%d/%d", s.ID, taskID, s.ResultsReceived, s.TotalTasks)
}

func (s *BuildState) AllResultsReceived() bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.ResultsReceived == s.TotalTasks
}

func (s *BuildState) GetResults() map[string]TaskResult {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	results := make(map[string]TaskResult)
	for k, v := range s.Results {
		results[k] = v
	}
	return results
}

// logTaskSummary 는 task 결과 요약을 로그에 남깁니다.
func (s *BuildState) logTaskSummary() {
	s.Mu.RLock()
	results := make(map[string]TaskResult, len(s.Results))
	for k, v := range s.Results {
		results[k] = v
	}
	taskArnByID := make(map[string]string, len(s.TaskArnByID))
	for k, v := range s.TaskArnByID {
		taskArnByID[k] = v
	}
	s.Mu.RUnlock()

	keys := make(map[string]struct{}, len(results)+len(taskArnByID))
	for k := range results {
		keys[k] = struct{}{}
	}
	for k := range taskArnByID {
		keys[k] = struct{}{}
	}

	taskIDs := make([]string, 0, len(keys))
	for k := range keys {
		taskIDs = append(taskIDs, k)
	}
	sort.Strings(taskIDs)

	for _, taskID := range taskIDs {
		result, ok := results[taskID]
		status := "unknown"
		errMsg := "-"
		if ok {
			if result.Success {
				status = "success"
			} else {
				status = "failed"
				if strings.TrimSpace(result.Error) != "" {
					errMsg = result.Error
				}
			}
		} else {
			errMsg = "result missing"
		}

		taskArn := taskArnByID[taskID]
		s.appendLog("info", fmt.Sprintf("[task-summary] task=%s arn=%s status=%s err=%s",
			taskID, taskArn, status, errMsg), true)
	}
}

// Finish 는 빌드를 종료하고 로그 채널을 닫습니다.
func (s *BuildState) Finish(err error) {
	s.Mu.Lock()

	if s.finished {
		s.Mu.Unlock()
		return
	}

	s.finished = true

	if s.FirstError != nil {
		err = s.FirstError
	} else if err != nil {
		s.FirstError = err
	}

	debugLog("[Finish] state=%s, err=%v, count=%d/%d", s.ID, err, s.ResultsReceived, s.TotalTasks)

	s.Mu.Unlock()

	s.logTaskSummary()

	if err != nil {
		s.appendLog("error", fmt.Sprintf("build finished with error: %v", err), true)
		s.appendLog("error", "BUILD FAILED", true)
	} else {
		s.appendLog("info", "build finished successfully", true)
		s.appendLog("info", "BUILD SUCCEEDED", true)
	}

	s.Mu.Lock()
	if !s.closed {
		close(s.Logs)
		close(s.Done)
		s.closed = true
	}
	s.Mu.Unlock()
}

func (s *BuildState) IsFinished() bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.finished
}

func (s *BuildState) SetError(err error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	if s.FirstError == nil && err != nil {
		s.FirstError = err
	}
}

func (s *BuildState) GetError() error {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.FirstError
}

func (s *BuildState) HasError() bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.FirstError != nil
}

func (s *BuildState) WaitResults(timeout time.Duration) bool {
	start := time.Now()
	for time.Since(start) < timeout {
		s.Mu.RLock()
		received := s.ResultsReceived
		total := s.TotalTasks
		s.Mu.RUnlock()

		if received >= total {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
