// Package sleeper is the Sleeper fantasy-football extension. This slice
// registers the extension and validates its config; tools, skills, and the
// UI land in later slices (see quack-extensions issue #93).
package sleeper

import (
	"bytes"
	"context"
	"fmt"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

const extensionName = "sleeper"

func init() {
	sdk.Register(extensionName, factory)
}

// config is this extension's own YAML shape, under extensions.sleeper.
// Enabled/DataDir mirror sdk.BaseConfig's reserved keys so KnownFields decoding below doesn't reject them.
type config struct {
	Enabled   *bool  `yaml:"enabled"`
	DataDir   string `yaml:"data_dir"`
	Snapshots string `yaml:"snapshots"`

	// DefaultUser is a Sleeper username or user_id; tools fall back to it
	// when a chat doesn't name one.
	DefaultUser string `yaml:"default_user"`

	// DefaultLeague lets the orchestrator omit league_id in chat.
	DefaultLeague string `yaml:"default_league"`

	// Season defaults to /v1/state/nfl's current season when zero.
	Season int `yaml:"season"`

	// Fixture serves the reference example JSON (marked Example) for any
	// artifact-backed card with no real chat yet, for demoing/QA before jobs exist.
	Fixture bool `yaml:"fixture"`
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	cfg := config{}
	if len(raw) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("sleeper: parse config: %w", err)
		}
	}
	switch cfg.Snapshots {
	case "", "off", "daily":
	default:
		return nil, fmt.Errorf("sleeper: snapshots must be \"off\" or \"daily\", got %q", cfg.Snapshots)
	}
	if cfg.Season < 0 {
		return nil, fmt.Errorf("sleeper: season must not be negative, got %d", cfg.Season)
	}

	client, err := NewClient(sleeperBaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sleeper: new client: %w", err)
	}
	return &extension{host: host, cfg: cfg, client: client}, nil
}

// sleeperBaseURL is the production API; tests build an extension directly
// with a Client pointed at cmd/qa-mock instead of going through factory.
const sleeperBaseURL = "https://api.sleeper.app"

type extension struct {
	host   sdk.Host
	cfg    config
	client *Client
}

var (
	_ sdk.Extension = (*extension)(nil)
	_ sdk.UI        = (*extension)(nil)
)

// Tools returns the read-only agent tools over the Sleeper client (issue #93).
func (e *extension) Tools() []tool.Tool {
	return []tool.Tool{
		e.userTool(),
		e.leagueTool(),
		e.rosterTool(),
		e.matchupTool(),
		e.standingsTool(),
		e.transactionsTool(),
		e.freeAgentsTool(),
		e.playerTool(),
		e.scheduleTool(),
		e.trendsTool(),
		e.draftTool(),
		e.historyTool(),
	}
}

// RegisterRoutes mounts the UI slice's served page and JSON API (ui.go); see mountUI.
func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) { e.mountUI(authed) }

// UI names the extension's nav entry; the page it points at renders every
// job's artifacts and launches new runs (docs/extensions/ui-kit.md).
func (e *extension) UI() sdk.UIDescriptor {
	return sdk.UIDescriptor{Title: "Sleeper", Href: "/sleeper/", Icon: "sports_football"}
}

var _ sdk.Starter = (*extension)(nil)

// Start runs the daily snapshot ticker when configured; it only ever
// fetches, never dispatches. No default_league means nothing to snapshot.
func (e *extension) Start(ctx context.Context) error {
	if e.cfg.Snapshots != "daily" || e.cfg.DefaultLeague == "" {
		return nil
	}
	go e.runSnapshotTicker(ctx)
	return nil
}
