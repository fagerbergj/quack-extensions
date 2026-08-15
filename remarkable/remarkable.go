// Package remarkable browses a self-hosted rmfakecloud instance's documents
// and dispatches the ones a user explicitly selects into quack's
// document-ingest workflow. See .quack/rmfakecloud-eval.md (agent-researcher
// repo) for why rmfakecloud's UI API - not its webhook, which carries no
// document identity - is the inbound path. Ingest is user-driven on purpose:
// every autosave of a note in progress bumps lastModified, so anything
// automatic runs the pipeline against half-written documents.
package remarkable

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "remarkable"

func init() {
	sdk.Register(extensionName, factory)
}

// config is this extension's own YAML shape, under extensions.remarkable in
// quack.yaml. It must not redefine BaseConfig's "enabled"/"data_dir" keys -
// quack reads those itself before Factory ever sees the bytes.
type config struct {
	// BaseURL is the rmfakecloud instance, e.g.
	// "https://remarkable.example.duckdns.org".
	BaseURL string `yaml:"base_url"`

	// Email/Password are the rmfakecloud UI account's login credentials -
	// what its /ui/api/login endpoint requires (no long-lived API token
	// exists; the extension re-logs in on 401).
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	cfg := config{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("remarkable: parse config: %w", err)
		}
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("remarkable: base_url is required")
	}
	if cfg.Email == "" || cfg.Password == "" {
		return nil, fmt.Errorf("remarkable: email and password are required")
	}
	if host.DataDir == "" {
		return nil, fmt.Errorf("remarkable: Host.DataDir is required")
	}

	return &extension{
		host:      host,
		client:    newRMClient(cfg.BaseURL, cfg.Email, cfg.Password, nil),
		statePath: statePath(host.DataDir),
	}, nil
}

type extension struct {
	host      sdk.Host
	client    *rmClient
	statePath string

	mu sync.Mutex
	st *state
}

var (
	_ sdk.Extension   = (*extension)(nil)
	_ sdk.Starter     = (*extension)(nil)
	_ sdk.RunObserver = (*extension)(nil)
	_ sdk.UI          = (*extension)(nil)
)

func (e *extension) Tools() []tool.Tool { return nil }

func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) {
	authed.Get("/documents", e.handleDocuments)
	authed.Post("/documents/ingest", e.handleIngest)
	authed.Get("/status", e.handleStatus)
	authed.Get("/status.json", e.handleStatusJSON)
}

// Start loads persisted state and does one synchronous login so an
// unreachable or misconfigured rmfakecloud fails startup loudly instead of
// idling silently. No background loop: ingest only happens when a user
// submits the documents page.
func (e *extension) Start(ctx context.Context) error {
	st, err := loadState(e.statePath)
	if err != nil {
		return fmt.Errorf("remarkable: load state: %w", err)
	}
	// Nothing is in flight at boot, by construction - a run can't outlive
	// the process that dispatched it.
	stale := false
	for id, ds := range st.Documents {
		if ds.InFlight {
			ds.InFlight = false
			ds.UpdatedAt = time.Now().UTC()
			st.Documents[id] = ds
			stale = true
		}
	}

	e.mu.Lock()
	e.st = st
	if stale {
		err = e.st.save(e.statePath)
	}
	e.mu.Unlock()
	if err != nil {
		e.host.Log.Error("remarkable: save state after clearing stale in-flight failed", "err", err)
	}

	if err := e.client.login(ctx); err != nil {
		return fmt.Errorf("remarkable: cannot reach rmfakecloud at %s: %w", e.client.baseURL, err)
	}
	return nil
}

// RunEnded clears InFlight and records the terminal outcome. quack passes
// the namespaced chat id ("ext:<extension>:<localID>"), so strip that back
// to the document ID the state file is keyed by.
func (e *extension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	docID := strings.TrimPrefix(chatID, "ext:"+extensionName+":")

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st == nil {
		return
	}
	ds, ok := e.st.Documents[docID]
	if !ok {
		return // not one of ours, or state was reset since dispatch
	}
	ds.InFlight = false
	ds.LastOutcome = string(outcome.Status)
	ds.UpdatedAt = time.Now().UTC()
	if outcome.Status == sdk.RunFailed {
		ds.LastError = outcome.Answer
	} else {
		ds.LastError = ""
	}
	e.st.Documents[docID] = ds

	if err := e.st.save(e.statePath); err != nil {
		e.host.Log.Error("remarkable: save state after run ended failed", "err", err, "doc_id", docID)
	}
}

func (e *extension) UI() sdk.UIDescriptor {
	return sdk.UIDescriptor{Title: "reMarkable", Href: documentsPath}
}
