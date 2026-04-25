package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	StatusPending          Status = "Pending"
	StatusBuilding         Status = "Building"
	StatusPushing          Status = "Pushing"
	StatusPulling          Status = "Pulling"
	StatusStarting         Status = "Starting"
	StatusHealthChecking   Status = "HealthChecking"
	StatusSwitchingTraffic Status = "SwitchingTraffic"
	StatusCleaningUp       Status = "CleaningUp"
	StatusSucceeded        Status = "Succeeded"
	StatusFailed           Status = "Failed"
	StatusRolledBack       Status = "RolledBack"
)

type Deployment struct {
	ID         string     `json:"id"`
	Service    string     `json:"service"`
	Version    string     `json:"version"`
	Image      string     `json:"image"`
	Status     Status     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Event struct {
	ID           string    `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	Host         string    `json:"host,omitempty"`
	Role         string    `json:"role,omitempty"`
	EventType    string    `json:"event_type"`
	Message      string    `json:"message"`
	CreatedAt    time.Time `json:"created_at"`
}

type record struct {
	Kind       string      `json:"kind"`
	Deployment *Deployment `json:"deployment,omitempty"`
	Event      *Event      `json:"event,omitempty"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			return nil, fmt.Errorf("create state file: %w", err)
		}
	}
	return &Store{path: path}, nil
}

func (s *Store) AppendDeployment(d Deployment) error {
	return s.append(record{Kind: "deployment", Deployment: &d})
}

func (s *Store) AppendEvent(e Event) error {
	return s.append(record{Kind: "event", Event: &e})
}

func (s *Store) append(r record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(r)
}

func (s *Store) Snapshot() ([]Deployment, []Event, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	deployments := map[string]Deployment{}
	events := []Event{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, nil, err
		}
		switch rec.Kind {
		case "deployment":
			deployments[rec.Deployment.ID] = *rec.Deployment
		case "event":
			events = append(events, *rec.Event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	out := make([]Deployment, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	sort.Slice(events, func(i, j int) bool {
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
	return out, events, nil
}

func (s *Store) LatestSuccessful(service string) (*Deployment, error) {
	deployments, _, err := s.Snapshot()
	if err != nil {
		return nil, err
	}
	var latest *Deployment
	for _, d := range deployments {
		if d.Service != service || d.Status != StatusSucceeded {
			continue
		}
		candidate := d
		if latest == nil || candidate.StartedAt.After(latest.StartedAt) {
			latest = &candidate
		}
	}
	if latest == nil {
		return nil, errors.New("no successful deployment found")
	}
	return latest, nil
}

