// Package github is quack's GitHub App extension: auth, tools, webhook
// dispatch - ported from quack's former internal/github (design doc
// .quack/design/sdk-v2-github.md, migration plan step 6).
package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const extensionName = "github"

// webhookPath is where the inbound webhook receiver is mounted, self-verifying via HMAC.
const webhookPath = "/webhook"

// runUserID distinguishes GitHub-driven sessions from local UI sessions
// when a comment carries no login (defensive; GitHub always sends one).
const runUserID = "github"

// defaultRunTimeout when run_timeout_minutes is unset.
const defaultRunTimeout = 2 * time.Hour

// reactionTimeout bounds the 👀 ack reaction on a mention.
const reactionTimeout = 10 * time.Second

func init() {
	sdk.Register(extensionName, factory)
}

// Labels names the label vocabulary quack:plan/implement/merge/etc map to in
// a specific deployment - the extension's own equivalent of quack's former
// config.GitHubLabels (that type is deleted from quack; this module now
// unmarshals its own config).
type Labels struct {
	Plan       string `yaml:"plan"`
	Implement  string `yaml:"implement"`
	Review     string `yaml:"review"`
	Merge      string `yaml:"merge"`
	PartialFix string `yaml:"partial_fix"`
	Fix        string `yaml:"fix"`
}

const (
	defaultMention         = "/quack"
	defaultAutoReviewLabel = "quack-auto-review"
	defaultPlanLabel       = "quack:plan"
	defaultImplementLabel  = "quack:implement"
	defaultMergeLabel      = "quack:merge"
	defaultPartialFixLabel = "quack:partial-fix"
	defaultFixLabel        = "quack:fix"
)

var validTriggers = map[string]bool{
	"mention": true, "pr_opened": true, "label": true,
	"issue_plan": true, "issue_implement": true, "merge": true,
	"ci_fix": true,
}

// config is this extension's own YAML shape, under extensions.github in
// quack.yaml - unchanged in shape from quack's former
// config.GitHubExtensionConfig, just no longer strict-parsed by quack
// itself (design doc's "Config surface: extensions.github:").
type config struct {
	ClientID           string   `yaml:"client_id"`
	AppID              int64    `yaml:"app_id"`
	PrivateKey         string   `yaml:"private_key"`
	PrivateKeyPath     string   `yaml:"private_key_path"`
	WebhookSecret      string   `yaml:"webhook_secret"`
	Mention            string   `yaml:"mention"`
	Triggers           []string `yaml:"triggers"`
	AutoReviewLabel    string   `yaml:"auto_review_label"`
	AllowedUsers       []string `yaml:"allowed_users"`
	Labels             Labels   `yaml:"labels"`
	RunTimeoutMinutes  int      `yaml:"run_timeout_minutes"`
	AutoArchiveOnMerge bool     `yaml:"auto_archive_on_merge"`
}

func (c *config) issuer() string {
	if c.ClientID != "" {
		return c.ClientID
	}
	return fmt.Sprintf("%d", c.AppID)
}

// applyDefaults validates and fills in defaults, mirroring quack's former
// GitHubExtensionConfig.applyDefaults exactly.
func (c *config) applyDefaults(log func(string, ...any)) error {
	switch {
	case c.ClientID == "" && c.AppID == 0:
		return fmt.Errorf("github: needs one of client_id (recommended) or app_id")
	case c.ClientID != "" && c.AppID != 0:
		return fmt.Errorf("github: sets both client_id and app_id; use one (client_id recommended)")
	}
	if c.PrivateKey == "" && c.PrivateKeyPath == "" {
		return fmt.Errorf("github: needs one of private_key or private_key_path")
	}
	if c.PrivateKey != "" && c.PrivateKeyPath != "" {
		return fmt.Errorf("github: sets both private_key and private_key_path; use one")
	}
	if c.WebhookSecret == "" {
		return fmt.Errorf("github: webhook_secret is required")
	}
	if c.RunTimeoutMinutes <= 0 {
		c.RunTimeoutMinutes = 120
	}
	if c.Mention == "" {
		c.Mention = defaultMention
	}
	if len(c.Triggers) == 0 {
		c.Triggers = []string{"mention"}
	}
	for _, t := range c.Triggers {
		if !validTriggers[t] {
			return fmt.Errorf("github: triggers has unknown entry %q (want mention, pr_opened, label, issue_plan, issue_implement, merge, or ci_fix)", t)
		}
	}
	if c.Labels.Review == "" {
		c.Labels.Review = c.AutoReviewLabel
	}
	if c.Labels.Review == "" {
		c.Labels.Review = defaultAutoReviewLabel
	}
	if c.Labels.Plan == "" {
		c.Labels.Plan = defaultPlanLabel
	}
	if c.Labels.Implement == "" {
		c.Labels.Implement = defaultImplementLabel
	}
	if c.Labels.Merge == "" {
		c.Labels.Merge = defaultMergeLabel
	}
	if c.Labels.PartialFix == "" {
		c.Labels.PartialFix = defaultPartialFixLabel
	}
	if c.Labels.Fix == "" {
		c.Labels.Fix = defaultFixLabel
	}
	if len(c.AllowedUsers) == 0 {
		log("github: allowed_users is empty; DENYING every human-invoked trigger " +
			"(mention comments, quack:plan/implement/merge labels) until it is set - auto-review is unaffected")
	}
	return nil
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	var cfg config
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("github: parse config: %w", err)
		}
	}
	logf := func(msg string, args ...any) {
		if host.Log != nil {
			host.Log.Warn(msg)
		}
	}
	if err := cfg.applyDefaults(logf); err != nil {
		return nil, err
	}
	if host.DataDir == "" {
		return nil, fmt.Errorf("github: Host.DataDir is required")
	}

	pem, err := LoadPrivateKey(cfg.PrivateKey, cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	app, err := NewApp(cfg.issuer(), pem)
	if err != nil {
		return nil, fmt.Errorf("github: init: %w", err)
	}
	app.SetPartialFixLabel(cfg.Labels.PartialFix)

	st, err := openStore(host.DataDir)
	if err != nil {
		return nil, err
	}

	triggers := make(map[string]bool, len(cfg.Triggers))
	for _, t := range cfg.Triggers {
		triggers[t] = true
	}
	allowedUsers := make(map[string]bool, len(cfg.AllowedUsers))
	for _, u := range cfg.AllowedUsers {
		allowedUsers[strings.ToLower(u)] = true
	}

	e := &Extension{
		app:                app,
		host:               host,
		store:              st,
		secret:             []byte(cfg.WebhookSecret),
		mention:            cfg.Mention,
		triggers:           triggers,
		labels:             cfg.Labels,
		allowedUsers:       allowedUsers,
		runTimeout:         time.Duration(cfg.RunTimeoutMinutes) * time.Minute,
		autoArchiveOnMerge: cfg.AutoArchiveOnMerge,
	}
	if host.Classify != nil {
		e.intentClassifier = hostClassifier{host: host}
	}
	return e, nil
}

// hostClassifier adapts Host.Classify to the IntentClassifier interface
// intent.go already codes against - Host.Classify didn't exist when that
// interface was written (quack's former SetIntentClassifier was a
// post-construction setter carrying a Go model object sdk.Factory's
// (Host, []byte) signature has no room for); this closes that gap now that
// the SDK has a single free-text classify call.
type hostClassifier struct{ host sdk.Host }

func (h hostClassifier) Classify(ctx context.Context, prompt string) (string, error) {
	return h.host.Classify(ctx, prompt)
}

// Extension is the GitHub App extension: tools + git auth + inbound webhook.
type Extension struct {
	app                *App
	host               sdk.Host
	store              *ghStore
	secret             []byte
	mention            string
	triggers           map[string]bool
	labels             Labels
	allowedUsers       map[string]bool // lower-cased; empty = deny all human-invoked triggers
	inflight           sync.Map        // sessionID → struct{}{}; dedup for concurrent triggers (#665, #668)
	mergeMu            keyedMutex      // serializes merge-intent read-verdict-act per session (Risk 2)
	pending            sync.Map        // globalChatID → *pendingRun; correlates RunEnded back to its dispatch
	runTimeout         time.Duration
	autoArchiveOnMerge bool

	// intentClassifier backs the mention-intent classification in intent.go -
	// wired to Host.Classify in factory when non-nil; nil (degrades to
	// conversational, intent.go's own documented fallback) when the
	// deployment's Host has no classify capability configured.
	intentClassifier IntentClassifier
}

var (
	_ sdk.Extension           = (*Extension)(nil)
	_ sdk.RunObserver         = (*Extension)(nil)
	_ sdk.Deliverer           = (*Extension)(nil)
	_ sdk.GitCredentialSource = (*Extension)(nil)
)

// isInvokerAllowed checks the configured allowlist. Empty list = deny all human-invoked triggers.
func (e *Extension) isInvokerAllowed(login string) bool {
	return e.allowedUsers[strings.ToLower(login)]
}

func (e *Extension) Tools() []tool.Tool { return e.app.Tools() }

// Deliver/GitCredential satisfy sdk.Deliverer/sdk.GitCredentialSource by
// delegating to App, which does the actual GitHub API work.
func (e *Extension) Deliver(ctx context.Context, dc sdk.DeliveryContext) ([]sdk.DeliveryItemOutcome, error) {
	return e.app.Deliver(ctx, dc)
}

func (e *Extension) GitCredential(ctx context.Context, rawURL string) (*sdk.GitCredential, error) {
	return e.app.GitCredential(ctx, rawURL)
}

// RegisterRoutes mounts the inbound webhook receiver on public - it verifies
// its own HMAC signature, same as it always has.
func (e *Extension) RegisterRoutes(authed chi.Router, public chi.Router) {
	public.Post(webhookPath, e.handleWebhook)
}

// globalChatID mirrors quack's own "ext:<extension>:<localID>" namespacing
// (sdk.ChatRef.LocalID's documented contract) - built locally so this
// extension can correlate a RunObserver.RunEnded callback (which arrives
// with the full namespaced id) back to the sessionID it dispatched.
func globalChatID(sessionID string) string {
	return "ext:" + extensionName + ":" + sessionID
}
