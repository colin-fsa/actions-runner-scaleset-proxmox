package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/store"
)

// quarantineJournal records a single held VM on an operator-provided durable
// volume. It carries no JIT config, token or customer data.
type quarantineJournal struct{ path string }

type quarantineRecord struct {
	VMID     int       `json:"vmid"`
	Node     string    `json:"node"`
	Name     string    `json:"name"`
	Profile  string    `json:"profile"`
	RunnerID int64     `json:"runner_id"`
	ReviewAt time.Time `json:"review_at"`
}

func newQuarantineJournal(dir, scaleSet string) (*quarantineJournal, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("quarantine journal directory must be an absolute path on durable storage")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("quarantine journal directory unavailable: %s", dir)
	}
	probe, err := os.CreateTemp(dir, ".quarantine-probe-*")
	if err != nil {
		return nil, fmt.Errorf("quarantine journal directory is not writable: %w", err)
	}
	probeName := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probeName)
		return nil, fmt.Errorf("close quarantine journal probe: %w", err)
	}
	if err := os.Remove(probeName); err != nil {
		return nil, fmt.Errorf("remove quarantine journal probe: %w", err)
	}
	hash := sha256.Sum256([]byte(scaleSet))
	return &quarantineJournal{path: filepath.Join(dir, "quarantine-"+hex.EncodeToString(hash[:8])+".json")}, nil
}

func (j *quarantineJournal) load() (*quarantineRecord, error) {
	b, err := os.ReadFile(j.path) //nolint:gosec // operator-configured journal path
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read quarantine journal: %w", err)
	}
	var rec quarantineRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("decode quarantine journal: %w", err)
	}
	if rec.VMID <= 0 || rec.Node == "" || rec.Name == "" || rec.ReviewAt.IsZero() {
		return nil, errors.New("invalid quarantine journal record")
	}
	return &rec, nil
}

func (j *quarantineJournal) save(row *store.VM, reviewAt time.Time) error {
	rec := quarantineRecord{VMID: row.VMID, Node: row.Node, Name: row.Name, Profile: row.Profile, RunnerID: row.RunnerID, ReviewAt: reviewAt}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(j.path), ".quarantine-*")
	if err != nil {
		return fmt.Errorf("create quarantine journal: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write quarantine journal: %w", err)
	}
	if err := os.Rename(name, j.path); err != nil {
		return fmt.Errorf("commit quarantine journal: %w", err)
	}
	dir, err := os.Open(filepath.Dir(j.path)) //nolint:gosec // operator-configured journal directory
	if err != nil {
		return fmt.Errorf("open quarantine journal directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync quarantine journal directory: %w", err)
	}
	return nil
}

func (j *quarantineJournal) remove(vmid int) error {
	rec, err := j.load()
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	if rec.VMID != vmid {
		return errors.New("quarantine journal belongs to a different VM")
	}
	if err := os.Remove(j.path); err != nil {
		return fmt.Errorf("remove quarantine journal: %w", err)
	}
	dir, err := os.Open(filepath.Dir(j.path)) //nolint:gosec // operator-configured journal directory
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
