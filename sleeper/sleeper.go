// Package sleeper is the Sleeper fantasy-football extension. This slice
// registers the extension and validates its config; tools, skills, and the
// UI land in later slices (see quack-extensions issue #93).
package sleeper

import (
	"bytes"
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

	return &extension{host: host, cfg: cfg}, nil
}

type extension struct {
	host sdk.Host
	cfg  config
}

var _ sdk.Extension = (*extension)(nil)

// Tools returns nil until the tool slice lands (issue #93 covers only the
// generated client and fixtures).
func (e *extension) Tools() []tool.Tool { return nil }

// RegisterRoutes is a no-op until the UI slice adds authed routes.
func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) {}
