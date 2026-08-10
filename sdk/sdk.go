// Package sdk is the quack extension API. It is self-contained: it must
// never import github.com/fagerbergj/quack, so an extension written against
// it can never accidentally depend on quack internals. quack imports this
// package (and each extension package) - never the reverse.
package sdk

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// Extension is what a module provides to quack.
type Extension interface {
	// Tools join every agent's tool set. Return nil/empty for an
	// inbound-only extension (routes/dispatch, no tools).
	Tools() []tool.Tool

	// RegisterRoutes mounts the extension's inbound routes. authed sits
	// behind quack's session auth (UI pages, extension APIs); public does
	// not (webhooks, which own their own verification - see Starter/Stopper
	// doc for the shared precedent). Either router may be left unused.
	RegisterRoutes(authed chi.Router, public chi.Router)
}

// Starter is an optional lifecycle interface for extensions that own
// background resources (watchers, pollers). Factories themselves must stay
// side-effect free - they also run in contexts that never serve, such as
// config validation - so resource acquisition belongs in Start, not the
// Factory.
type Starter interface {
	Start(ctx context.Context) error
}

// Stopper is the Starter counterpart. Stop must be idempotent: quack may
// call it during shutdown paths that don't guarantee Start ran, or don't
// guarantee Stop runs exactly once.
type Stopper interface {
	Stop(ctx context.Context) error
}

// RunStatus is the terminal outcome of a dispatched run, reported to
// RunObserver.
type RunStatus string

const (
	RunDone       RunStatus = "done"
	RunFailed     RunStatus = "failed"
	RunNeedsInput RunStatus = "needs_input"
)

// RunObserver is an optional, observation-only interface: quack calls
// RunEnded after a dispatched run's outcome is final, so an extension can
// mark its own records done/failed and drive retries. It never blocks or
// mutates the run - dispatch-time shaping (DispatchRequest) is the only
// place an extension influences a run; there are no agent-loop hooks.
type RunObserver interface {
	RunEnded(chatID string, status RunStatus)
}

// Host is what quack hands an extension's Factory.
type Host struct {
	// Dispatch starts a run, or appends a turn to one already in progress -
	// see DispatchRequest.ChatID.
	Dispatch DispatchFunc

	// Log is pre-tagged component=ext.<name>; extensions should log through
	// it rather than building their own handler.
	Log *slog.Logger

	// DataDir is extension-private persistent storage, e.g. a SQLite
	// registry mapping external documents to chat ids. Extensions own its
	// contents; quack only guarantees the directory exists and is theirs
	// alone.
	DataDir string
}

// DispatchFunc starts or continues a run. See DispatchRequest.ChatID for the
// new-chat-vs-append-a-turn semantics.
type DispatchFunc func(ctx context.Context, req DispatchRequest) error

// DispatchRequest is the dispatch-time shape of a run. This is the entire
// surface an extension has to influence a run - there are no agent-loop
// hooks (see the package doc): everything here is decided once, before the
// workflow starts.
type DispatchRequest struct {
	// ChatID is a stable identity chosen by the extension, e.g.
	// "remarkable-<doc-id>". Dispatching to a ChatID that already has a
	// chat appends a turn to it (the GitHub webhook's resume semantics);
	// a ChatID quack hasn't seen starts a fresh chat.
	ChatID string

	// User is the ADK session identity the run executes as.
	User string

	// Message is the ask the workflow sees - the turn content.
	Message string

	// Background is planning-scale context, envelope-style: the evidence a
	// planner needs but that shouldn't crowd out a node's own ask.
	Background string

	// Workflow names a workflow-catalog shape, e.g. "document-ingest".
	// Empty selects quack's default planner-driven flow.
	Workflow string

	// Attachments are delivered alongside Message.
	Attachments []Attachment

	// Origin is sidebar provenance: how the chat should present itself and
	// group with others from the same extension. Nil is fine - the chat
	// just renders with no origin chip.
	Origin *ChatOrigin
}

// Attachment is a byte payload delivered with a DispatchRequest.
type Attachment struct {
	Name string
	MIME string
	Data []byte
}

// ChatOrigin is generic provenance the SPA renders without knowing the
// extension that produced it: a label chip, an optional badge and external
// link, and facet grouping by extension/kind.
type ChatOrigin struct {
	Extension string // registration name, e.g. "remarkable"
	Label     string // human handle, e.g. the doc title or "owner/repo#42"
	Kind      string // facet group, e.g. "document", "pr", "issue"
	URL       string // optional external link
	Badge     string // optional short status chip, e.g. "draft"
}

// Factory builds an extension. config is the raw bytes of the deployment's
// extensions.<name> block - the extension unmarshals its own config; quack
// treats extension blocks as opaque. Factories must be side-effect free:
// validate config and construct, nothing more (see Starter/Stopper).
type Factory func(host Host, config []byte) (Extension, error)
