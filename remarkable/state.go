package remarkable

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "state.json"

// docState is the per-document record the poller diffs against: what we
// last saw and what happened when we last dispatched it.
type docState struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Folder       string    `json:"folder,omitempty"`
	LastModified time.Time `json:"last_modified"`

	// InFlight is true from a successful Dispatch call until RunEnded
	// fires - it suppresses a re-dispatch of a run still in progress.
	InFlight bool `json:"in_flight"`

	// LastOutcome/LastError describe the most recent finished attempt.
	// LastOutcome == "failed" (and not InFlight) is what makes pollOnce
	// retry the document on the next cycle - exactly once per cycle,
	// since each document is visited once per poll.
	LastOutcome string `json:"last_outcome,omitempty"`
	LastError   string `json:"last_error,omitempty"`

	// GaveUp is true once Attempts has reached the configured cap for this
	// LastModified value - the poller stops retrying until the document
	// changes again. Set alongside the one-time Warn log.
	GaveUp bool `json:"gave_up"`

	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}

const outcomeFailed = "failed"

// state is the whole extension-private record, persisted as one JSON file
// in Host.DataDir. Single writer (the poll loop, plus RunEnded callbacks
// serialized through poller.mu) - no need for sqlite.
type state struct {
	Documents map[string]docState `json:"documents"`
	LastPoll  time.Time           `json:"last_poll"`
}

func loadState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &state{Documents: map[string]docState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.Documents == nil {
		st.Documents = map[string]docState{}
	}
	return &st, nil
}

// save writes atomically (tmp file + rename) so a crash mid-write can never
// leave a half-written state.json behind.
func (s *state) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func statePath(dataDir string) string {
	return filepath.Join(dataDir, stateFileName)
}
