package remarkable

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// statusResponse mirrors noop's /status pattern: an authed JSON snapshot,
// not a webhook (there is none for the reMarkable poller).
type statusResponse struct {
	Extension string      `json:"extension"`
	BaseURL   string      `json:"base_url"`
	LastPoll  time.Time   `json:"last_poll"`
	Documents []statusDoc `json:"documents"`
}

type statusDoc struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Folder       string    `json:"folder,omitempty"`
	LastModified time.Time `json:"last_modified"`
	InFlight     bool      `json:"in_flight"`
	LastOutcome  string    `json:"last_outcome,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	GaveUp       bool      `json:"gave_up"`
	Attempts     int       `json:"attempts"`
}

func (e *extension) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := e.poller.snapshot()

	resp := statusResponse{
		Extension: extensionName,
		BaseURL:   e.poller.client.baseURL,
		LastPoll:  snap.LastPoll,
		Documents: make([]statusDoc, 0, len(snap.Documents)),
	}
	for _, d := range snap.Documents {
		resp.Documents = append(resp.Documents, statusDoc{
			ID:           d.ID,
			Name:         d.Name,
			Folder:       d.Folder,
			LastModified: d.LastModified,
			InFlight:     d.InFlight,
			LastOutcome:  d.LastOutcome,
			LastError:    d.LastError,
			GaveUp:       d.GaveUp,
			Attempts:     d.Attempts,
		})
	}
	sort.Slice(resp.Documents, func(i, j int) bool { return resp.Documents[i].ID < resp.Documents[j].ID })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
