package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Job struct {
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	Args        []string   `json:"args,omitempty"`
	Workdir     string     `json:"workdir,omitempty"`
	Status      Status     `json:"status"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	OutputPath  string     `json:"output_path"`
	LastError   string     `json:"last_error,omitempty"`
}

type Manager struct {
	mu        sync.RWMutex
	jobs      map[string]*Job
	dataPath  string
	outputDir string
	tailLines int
}

type Config struct {
	DataDir   string
	TailLines int
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("data directory is required")
	}
	if cfg.TailLines <= 0 {
		cfg.TailLines = 20
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	outputDir := filepath.Join(cfg.DataDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	mgr := &Manager{
		jobs:      map[string]*Job{},
		dataPath:  filepath.Join(cfg.DataDir, "jobs.json"),
		outputDir: outputDir,
		tailLines: cfg.TailLines,
	}

	if err := mgr.load(); err != nil {
		return nil, err
	}
	mgr.recoverRunning()
	return mgr, nil
}

func (m *Manager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	f, err := os.Open(m.dataPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	var stored map[string]*Job
	if err := decoder.Decode(&stored); err != nil {
		return fmt.Errorf("decode data file: %w", err)
	}
	if stored == nil {
		stored = make(map[string]*Job)
	}
	m.jobs = stored
	return nil
}

func (m *Manager) recoverRunning() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job.Status == StatusRunning {
			job.Status = StatusFailed
			job.LastError = "server restarted while job was running"
			now := time.Now().UTC()
			job.CompletedAt = &now
			code := -1
			job.ExitCode = &code
		}
	}
	_ = m.saveLocked()
}

func (m *Manager) CreateJob(command string, args []string, workdir string) (*Job, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("command is required")
	}
	if workdir != "" {
		info, err := os.Stat(workdir)
		if err != nil {
			return nil, fmt.Errorf("workdir: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workdir is not a directory: %s", workdir)
		}
	}
	id := newID()
	job := &Job{
		ID:         id,
		Command:    command,
		Args:       append([]string{}, args...),
		Workdir:    workdir,
		Status:     StatusQueued,
		CreatedAt:  time.Now().UTC(),
		OutputPath: filepath.Join(m.outputDir, fmt.Sprintf("%s.log", id)),
	}

	m.mu.Lock()
	m.jobs[job.ID] = job
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()

	go m.run(job)
	return m.copyJob(job), nil
}

func (m *Manager) Get(id string) (*Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return m.copyJob(job), nil
}

func (m *Manager) copyJob(job *Job) *Job {
	if job == nil {
		return nil
	}
	clone := *job
	clone.Args = append([]string{}, job.Args...)
	return &clone
}

func (m *Manager) Tail(id string) ([]string, error) {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return tailFile(job.OutputPath, m.tailLines)
}

func (m *Manager) run(job *Job) {
	outputFile := filepath.Join(m.outputDir, fmt.Sprintf("%s.log", job.ID))
	if err := m.updateJob(job.ID, func(j *Job) {
		now := time.Now().UTC()
		j.Status = StatusRunning
		j.StartedAt = &now
		j.OutputPath = outputFile
	}); err != nil {
		return
	}

	f, err := os.Create(outputFile)
	if err != nil {
		_ = m.markFailed(job.ID, -1, fmt.Sprintf("create log: %v", err))
		return
	}
	defer f.Close()

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, job.Command, job.Args...)
	if job.Workdir != "" {
		cmd.Dir = job.Workdir
	}
	cmd.Stdout = f
	cmd.Stderr = f

	if err := cmd.Start(); err != nil {
		_ = m.markFailed(job.ID, -1, fmt.Sprintf("start: %v", err))
		return
	}

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		exitCode = extractExitCode(waitErr)
		_ = m.markFailed(job.ID, exitCode, waitErr.Error())
		return
	}

	_ = m.updateJob(job.ID, func(j *Job) {
		j.Status = StatusSucceeded
		j.LastError = ""
		j.ExitCode = intPtr(exitCode)
		now := time.Now().UTC()
		j.CompletedAt = &now
	})
}

func (m *Manager) markFailed(id string, code int, errMsg string) error {
	return m.updateJob(id, func(j *Job) {
		j.Status = StatusFailed
		j.LastError = errMsg
		j.ExitCode = intPtr(code)
		now := time.Now().UTC()
		j.CompletedAt = &now
	})
}

func (m *Manager) updateJob(id string, mutate func(*Job)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return os.ErrNotExist
	}
	mutate(job)
	return m.saveLocked()
}

func (m *Manager) saveLocked() error {
	tmp := m.dataPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp data file: %w", err)
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(m.jobs); err != nil {
		f.Close()
		return fmt.Errorf("encode data: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp data: %w", err)
	}
	if err := os.Rename(tmp, m.dataPath); err != nil {
		return fmt.Errorf("rename temp data: %w", err)
	}
	return nil
}

func tailFile(path string, maxLines int) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= maxLines {
		return lines, nil
	}
	return lines[len(lines)-maxLines:], nil
}

func extractExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(interface{ ExitStatus() int }); ok {
			return status.ExitStatus()
		}
	}
	return -1
}

func newID() string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func intPtr(v int) *int { return &v }
