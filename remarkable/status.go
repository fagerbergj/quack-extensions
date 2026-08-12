package remarkable

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"time"
)

// statusResponse is the poller snapshot shared by both /status (HTML, the
// page linked from the SPA nav via sdk.UI) and /status.json (the same data,
// unrendered, for tooling/noop-style polling).
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

// Badge is the .qk-badge modifier for this row - "" is the neutral
// (never-run) case, deliberately not "ok".
func (d statusDoc) Badge() string {
	switch {
	case d.InFlight:
		return "warn"
	case d.GaveUp, d.LastOutcome == "failed":
		return "err"
	case d.LastOutcome == "done":
		return "ok"
	case d.LastOutcome == "needs_input":
		return "warn"
	default:
		return ""
	}
}

// Label is the badge's visible text.
func (d statusDoc) Label() string {
	switch {
	case d.InFlight:
		return "in flight"
	case d.GaveUp:
		return "gave up"
	case d.LastOutcome == "":
		return "never run"
	default:
		return d.LastOutcome
	}
}

func (r statusResponse) LastPollDisplay() string {
	if r.LastPoll.IsZero() {
		return "never"
	}
	return r.LastPoll.Format("2006-01-02 15:04:05 MST")
}

func (e *extension) statusSnapshot() statusResponse {
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
	return resp
}

// handleStatus renders the human-facing page (sdk.UI's Href target). JSON
// moved to handleStatusJSON - a path split, not Accept-header negotiation,
// since there's exactly one consumer of each.
func (e *extension) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, e.statusSnapshot()); err != nil {
		e.host.Log.Error("remarkable: status page render failed", "err", err)
	}
}

func (e *extension) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(e.statusSnapshot()); err != nil {
		e.host.Log.Error("remarkable: encode status JSON failed", "err", err)
	}
}

// statusKitCSS: this module still pins an sdk release older than
// sdk.UIKitCSS (v0.2.3), so the frozen v1 path is inlined - swap for the
// constant once go.mod bumps.
const statusKitCSS = "/assets/ext/v1/kit.css"

var statusTmpl = template.Must(template.New("status").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>reMarkable - quack</title>
<link rel="stylesheet" href="` + statusKitCSS + `">
</head>
<body class="qk-page">
<div class="qk-page__inner">
  <div class="qk-page__header">
    <div>
      <h1>reMarkable</h1>
      <p>{{.BaseURL}} &middot; last poll {{.LastPollDisplay}}</p>
    </div>
    <a class="qk-btn" href="/remarkable/status.json">JSON</a>
  </div>
  <div class="qk-table-wrap">
    <table class="qk-table">
      <thead><tr><th>Document</th><th>Folder</th><th>Modified</th><th>Attempts</th><th>Status</th></tr></thead>
      <tbody>
      {{range .Documents}}
        <tr>
          <td>{{.Name}}</td>
          <td>{{.Folder}}</td>
          <td>{{.LastModified.Format "2006-01-02 15:04"}}</td>
          <td>{{.Attempts}}</td>
          <td>
            <span class="qk-badge{{with .Badge}} qk-badge--{{.}}{{end}}">{{.Label}}</span>
            {{with .LastError}}<span style="color:var(--qk-muted)">{{.}}</span>{{end}}
          </td>
        </tr>
      {{else}}
        <tr><td colspan="5">No documents seen yet.</td></tr>
      {{end}}
      </tbody>
    </table>
  </div>
</div>
</body>
</html>
`))
