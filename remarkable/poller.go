package remarkable

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const workflowDocumentIngest = "document-ingest"

// poller is the Starter/Stopper background loop: list -> diff against
// state -> export PDF -> Dispatch. One poller per extension instance.
type poller struct {
	host         sdk.Host
	client       *rmClient
	interval     time.Duration
	folderFilter string
	statePath    string

	// maxAttempts caps retries of a permanently-failing document (real
	// case: quack's model layer rejecting application/pdf until PDF
	// decoding lands - without a cap that re-dispatches, and re-runs the
	// LLM pipeline, once per poll interval forever). 0 = unlimited.
	maxAttempts int

	mu sync.Mutex
	st *state

	stopFn  context.CancelFunc
	stopped chan struct{}
	// guards double-cancel from a concurrent or repeated Stop call
	stopOnce sync.Once
}

// Start loads persisted state and does one synchronous login so an
// unreachable or misconfigured rmfakecloud fails startup loudly instead of
// idling silently; the poll loop itself then runs on its own context so it
// survives Start's context being scoped narrowly by the caller.
func (p *poller) Start(ctx context.Context) error {
	st, err := loadState(p.statePath)
	if err != nil {
		return fmt.Errorf("remarkable: load state: %w", err)
	}
	p.mu.Lock()
	p.st = st
	p.mu.Unlock()

	if err := p.client.login(ctx); err != nil {
		return fmt.Errorf("remarkable: cannot reach rmfakecloud at %s: %w", p.client.baseURL, err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	p.stopFn = cancel
	p.stopped = make(chan struct{})
	go p.run(runCtx)
	return nil
}

// Stop is idempotent: quack may call it even when Start never ran, or call
// it more than once during shutdown.
func (p *poller) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		if p.stopFn != nil {
			p.stopFn()
		}
	})
	if p.stopped == nil {
		return nil
	}
	select {
	case <-p.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *poller) run(ctx context.Context) {
	defer close(p.stopped)

	p.pollOnce(ctx)

	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *poller) pollOnce(ctx context.Context) {
	docs, err := p.client.listDocuments(ctx)
	if err != nil {
		p.host.Log.Error("remarkable: list documents failed", "err", err)
		return
	}

	p.mu.Lock()
	for _, d := range docs {
		if !matchesFolder(d.Folder, p.folderFilter) {
			continue
		}
		if !p.needsDispatchLocked(d) {
			continue
		}
		p.dispatchLocked(ctx, d)
	}
	p.st.LastPoll = time.Now().UTC()
	err = p.st.save(p.statePath)
	p.mu.Unlock()

	if err != nil {
		p.host.Log.Error("remarkable: save state failed", "err", err)
	}
}

// needsDispatchLocked decides new/updated/retry-once-per-cycle, capped by
// maxAttempts. Callers must hold p.mu.
func (p *poller) needsDispatchLocked(d remoteDoc) bool {
	existing, seen := p.st.Documents[d.ID]
	if !seen {
		return true
	}
	if !existing.LastModified.Equal(d.LastModified) {
		return true // new version: always try, regardless of a prior give-up
	}
	if existing.InFlight {
		return false // still running from a previous cycle
	}
	if existing.LastOutcome != outcomeFailed {
		return false // done / needs_input: no retry
	}
	if p.maxAttempts > 0 && existing.Attempts >= p.maxAttempts {
		if !existing.GaveUp {
			p.giveUpLocked(d, existing)
		}
		return false
	}
	return true // failed, unchanged, under the cap: exactly one retry this cycle
}

// giveUpLocked marks a document as no longer retried at its current
// LastModified and logs once - the caller has already confirmed this is
// the first cycle crossing the cap. Callers must hold p.mu; the resulting
// state change is persisted by pollOnce's end-of-cycle save.
func (p *poller) giveUpLocked(d remoteDoc, existing docState) {
	existing.GaveUp = true
	p.st.Documents[d.ID] = existing
	p.host.Log.Warn("remarkable: giving up on document after repeated failures",
		"doc_id", d.ID, "name", d.Name, "attempts", existing.Attempts,
		"max_attempts", p.maxAttempts, "last_error", existing.LastError)
}

// dispatchLocked downloads the PDF and calls Host.Dispatch. Callers must
// hold p.mu. A download or dispatch error is recorded as an immediate
// failure (not InFlight) so the next poll retries it; a successful Dispatch
// call marks the doc InFlight until RunEnded reports the outcome.
func (p *poller) dispatchLocked(ctx context.Context, d remoteDoc) {
	pdf, err := p.client.downloadPDF(ctx, d.ID)
	if err != nil {
		p.host.Log.Error("remarkable: download failed", "doc_id", d.ID, "err", err)
		p.recordFailureLocked(d, err)
		return
	}

	req := sdk.DispatchRequest{
		Chat: sdk.ChatRef{
			LocalID: d.ID,
			Title:   d.Name,
			Origin: &sdk.ChatOrigin{
				Extension: extensionName,
				Label:     d.Name,
				Kind:      "document",
				Labels:    buildLabels(d),
			},
		},
		Ask: sdk.Ask{
			Message: fmt.Sprintf("A reMarkable document %q has arrived for ingest.", d.Name),
			Attachments: []sdk.Attachment{{
				// Doc ID, not visibleName: a rename must not change the
				// attachment name that ties document versions together.
				Name: d.ID + ".pdf",
				MIME: "application/pdf",
				Data: pdf,
			}},
		},
		Run: sdk.RunConfig{Workflow: workflowDocumentIngest},
	}

	if err := p.host.Dispatch(ctx, req); err != nil {
		p.host.Log.Error("remarkable: dispatch failed", "doc_id", d.ID, "err", err)
		p.recordFailureLocked(d, err)
		return
	}

	p.st.Documents[d.ID] = docState{
		ID:           d.ID,
		Name:         d.Name,
		Folder:       d.Folder,
		LastModified: d.LastModified,
		InFlight:     true,
		Attempts:     p.nextAttemptsLocked(d),
		UpdatedAt:    time.Now().UTC(),
	}
}

func (p *poller) recordFailureLocked(d remoteDoc, cause error) {
	p.st.Documents[d.ID] = docState{
		ID:           d.ID,
		Name:         d.Name,
		Folder:       d.Folder,
		LastModified: d.LastModified,
		InFlight:     false,
		LastOutcome:  outcomeFailed,
		LastError:    cause.Error(),
		Attempts:     p.nextAttemptsLocked(d),
		UpdatedAt:    time.Now().UTC(),
	}
}

// nextAttemptsLocked is the attempt count this dispatch represents: 1 for a
// document we've never seen, or whose LastModified just changed (a new
// version resets the cap); otherwise the previous count plus one. Callers
// must hold p.mu and call this before overwriting p.st.Documents[d.ID].
func (p *poller) nextAttemptsLocked(d remoteDoc) int {
	prev, seen := p.st.Documents[d.ID]
	if !seen || !prev.LastModified.Equal(d.LastModified) {
		return 1
	}
	return prev.Attempts + 1
}

// runEnded is the RunObserver callback: it clears InFlight and records the
// terminal outcome. A failed run leaves the doc eligible for exactly one
// retry on the next poll (needsDispatchLocked); needs_input and done do not
// retry - the chat is parked awaiting a human, or finished.
func (p *poller) runEnded(chatID string, outcome sdk.RunOutcome) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ds, ok := p.st.Documents[chatID]
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
	p.st.Documents[chatID] = ds

	if err := p.st.save(p.statePath); err != nil {
		p.host.Log.Error("remarkable: save state after run ended failed", "err", err, "doc_id", chatID)
	}
}

// snapshot returns a defensive copy of current state for /status.
func (p *poller) snapshot() state {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil {
		return state{Documents: map[string]docState{}}
	}
	out := state{Documents: make(map[string]docState, len(p.st.Documents)), LastPoll: p.st.LastPoll}
	for k, v := range p.st.Documents {
		out.Documents[k] = v
	}
	return out
}

// buildLabels carries folder as a Labels dimension. No "tags" dimension:
// rmfakecloud's GET /ui/api/documents never surfaces tags (Document has no
// such field, and /documents/:id/metadata is an unimplemented stub as of
// v0.0.31) - an rmfakecloud limitation, not an SDK one.
func buildLabels(d remoteDoc) map[string][]sdk.LabelValue {
	if d.Folder == "" {
		return nil
	}
	return map[string][]sdk.LabelValue{
		"folder": {{Value: d.Folder, Display: d.Folder}},
	}
}

func matchesFolder(folder, filter string) bool {
	if filter == "" {
		return true
	}
	return folder == filter || strings.HasPrefix(folder, filter+"/")
}
