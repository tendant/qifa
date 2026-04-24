package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Status string

var ErrNoActiveTarget = errors.New("no active target found")

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

type ActiveTarget struct {
	Service      string    `json:"service"`
	Host         string    `json:"host"`
	Role         string    `json:"role"`
	DeploymentID string    `json:"deployment_id"`
	Version      string    `json:"version"`
	Image        string    `json:"image"`
	Container    string    `json:"container"`
	TargetHost   string    `json:"target_host,omitempty"`
	TargetPort   int       `json:"target_port,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type record struct {
	Kind         string        `json:"kind"`
	Deployment   *Deployment   `json:"deployment,omitempty"`
	Event        *Event        `json:"event,omitempty"`
	ActiveTarget *ActiveTarget `json:"active_target,omitempty"`
}

type Store struct {
	path string
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

func (s *Store) AppendActiveTarget(a ActiveTarget) error {
	return s.append(record{Kind: "active_target", ActiveTarget: &a})
}

func (s *Store) append(r record) error {
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(r)
}

func (s *Store) Snapshot() ([]Deployment, []Event, error) {
	deployments, events, _, err := s.snapshot()
	return deployments, events, err
}

func (s *Store) snapshot() ([]Deployment, []Event, []ActiveTarget, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()

	deployments := map[string]Deployment{}
	events := []Event{}
	activeTargets := map[string]ActiveTarget{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, nil, nil, err
		}
		switch rec.Kind {
		case "deployment":
			deployments[rec.Deployment.ID] = *rec.Deployment
		case "event":
			events = append(events, *rec.Event)
		case "active_target":
			key := activeTargetKey(rec.ActiveTarget.Host, rec.ActiveTarget.Role)
			activeTargets[key] = *rec.ActiveTarget
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, nil, err
	}
	out := make([]Deployment, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, d)
	}
	targets := make([]ActiveTarget, 0, len(activeTargets))
	for _, target := range activeTargets {
		targets = append(targets, target)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	sort.Slice(events, func(i, j int) bool {
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].UpdatedAt.Before(targets[j].UpdatedAt)
	})
	return out, events, targets, nil
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

func (s *Store) RollbackTarget(service string) (*Deployment, error) {
	deployments, _, activeTargets, err := s.snapshot()
	if err != nil {
		return nil, err
	}

	activeVersions := map[string]struct{}{}
	for _, target := range activeTargets {
		if target.Service == service {
			activeVersions[target.Version] = struct{}{}
		}
	}

	successes := make([]Deployment, 0)
	for _, deployment := range deployments {
		if deployment.Service == service && deployment.Status == StatusSucceeded {
			successes = append(successes, deployment)
		}
	}
	if len(successes) == 0 {
		return nil, errors.New("no successful deployment found")
	}

	sort.Slice(successes, func(i, j int) bool {
		return successes[i].StartedAt.After(successes[j].StartedAt)
	})
	for _, success := range successes {
		if _, ok := activeVersions[success.Version]; ok {
			continue
		}
		target := success
		return &target, nil
	}
	target := successes[0]
	return &target, nil
}

func (s *Store) ActiveTargets(service string) ([]ActiveTarget, error) {
	_, _, targets, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	filtered := make([]ActiveTarget, 0, len(targets))
	for _, target := range targets {
		if target.Service == service {
			filtered = append(filtered, target)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
	})
	return filtered, nil
}

func (s *Store) ActiveTarget(service, host, role string) (*ActiveTarget, error) {
	targets, err := s.ActiveTargets(service)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		if target.Host == host && target.Role == role {
			candidate := target
			return &candidate, nil
		}
	}
	return nil, ErrNoActiveTarget
}

func activeTargetKey(host, role string) string {
	return host + "\x00" + role
}
