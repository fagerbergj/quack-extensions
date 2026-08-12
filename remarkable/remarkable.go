// Package remarkable polls a self-hosted rmfakecloud instance for reMarkable
// tablet documents and dispatches each new or updated one into quack's
// document-ingest workflow. See .quack/rmfakecloud-eval.md (agent-researcher
// repo) for why polling rmfakecloud's UI API - not its webhook, which
// carries no document identity - is the inbound path.
package remarkable

import (
	"context"
	"fmt"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "remarkable"

const defaultPollInterval = time.Minute

// defaultMaxAttempts caps retries of a permanently-failing document so a
// bad doc can't become a runaway per-poll-interval LLM-cost loop. 0 means
// unlimited and must never be the default - it's opt-in only.
const defaultMaxAttempts = 3

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

	// PollInterval is a Go duration string, e.g. "1m", "90s". Defaults to
	// one minute.
	PollInterval string `yaml:"poll_interval"`

	// FolderFilter, if set, restricts polling to this folder path and its
	// subfolders (e.g. "Inbox" matches "Inbox" and "Inbox/Scans"). Empty
	// means all folders.
	FolderFilter string `yaml:"folder_filter"`

	// MaxAttempts caps retries of a document that keeps failing at the same
	// LastModified value. A pointer so an absent key defaults to
	// defaultMaxAttempts while an explicit "max_attempts: 0" means
	// unlimited - the same absent-vs-explicit-zero pattern as
	// sdk.BaseConfig.Enabled.
	MaxAttempts *int `yaml:"max_attempts"`
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

	interval := defaultPollInterval
	if cfg.PollInterval != "" {
		parsed, err := time.ParseDuration(cfg.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("remarkable: invalid poll_interval %q: %w", cfg.PollInterval, err)
		}
		if parsed <= 0 {
			return nil, fmt.Errorf("remarkable: poll_interval must be positive, got %q", cfg.PollInterval)
		}
		interval = parsed
	}

	maxAttempts := defaultMaxAttempts
	if cfg.MaxAttempts != nil {
		if *cfg.MaxAttempts < 0 {
			return nil, fmt.Errorf("remarkable: max_attempts must be >= 0, got %d", *cfg.MaxAttempts)
		}
		maxAttempts = *cfg.MaxAttempts
	}

	p := &poller{
		host:         host,
		client:       newRMClient(cfg.BaseURL, cfg.Email, cfg.Password, nil),
		interval:     interval,
		folderFilter: cfg.FolderFilter,
		maxAttempts:  maxAttempts,
		statePath:    statePath(host.DataDir),
	}

	return &extension{host: host, poller: p}, nil
}

type extension struct {
	host   sdk.Host
	poller *poller
}

var (
	_ sdk.Extension   = (*extension)(nil)
	_ sdk.Starter     = (*extension)(nil)
	_ sdk.Stopper     = (*extension)(nil)
	_ sdk.RunObserver = (*extension)(nil)
	_ sdk.UI          = (*extension)(nil)
)

func (e *extension) Tools() []tool.Tool { return nil }

func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) {
	authed.Get("/status", e.handleStatus)
	authed.Get("/status.json", e.handleStatusJSON)
}

func (e *extension) Start(ctx context.Context) error { return e.poller.Start(ctx) }
func (e *extension) Stop(ctx context.Context) error  { return e.poller.Stop(ctx) }

func (e *extension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	e.poller.runEnded(chatID, outcome)
}

func (e *extension) UI() sdk.UIDescriptor {
	return sdk.UIDescriptor{Title: "reMarkable", Href: "/remarkable/status"}
}
