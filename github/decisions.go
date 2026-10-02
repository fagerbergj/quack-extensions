package github

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// Decision points run in observe mode only: answers are recorded by the host
// and never change what this extension posts.

// decideTimeout bounds each Decide call and each GitHub read the state needs.
var decideTimeout = 10 * time.Second

// Caps keep a state well under the handler's 8192-token input cap.
const (
	hunkCap        = 4000
	findingCap     = 4000
	prBodyCap      = 2000
	rationaleCap   = 6000
	findingsBudget = 12000
)

// reviewLabelRe mirrors quack-core's commentLabelRe, the label set quack counts toward a verdict.
var reviewLabelRe = regexp.MustCompile(`(?i)^\s*[^\pL\pN*]{0,4}\*{0,2}(blocking|suggestion|nit|question)\b[^:]*:\*{0,2}`)

// carriedOverRe matches the prefix quack puts on a finding carried over from a prior review.
var carriedOverRe = regexp.MustCompile(`^\(carried over[^)]*\)\s*`)

// verdictLineRe matches the review overview's "**Verdict: ...**" line, the baseline in prose.
var verdictLineRe = regexp.MustCompile(`(?m)^\*\*Verdict:[^\n]*\n*`)

var (
	severityQuestions = map[string]sdk.DecisionQuestion{"severity": {
		Type:         "choice",
		Instructions: "Which Conventional Comments label fits this code-review finding, given the diff hunk it is anchored to?",
		Criteria: map[string]string{
			"blocking":   "must be fixed before the pull request merges",
			"suggestion": "a worthwhile improvement that need not block the merge",
			"nit":        "trivial style or polish",
			"question":   "asks for clarification rather than requesting a change",
		},
	}}
	blockingQuestions = map[string]sdk.DecisionQuestion{"blocking": {
		Type:         "noul",
		Instructions: "Would merging with this finding unaddressed be a mistake?",
	}}
	verdictQuestions = map[string]sdk.DecisionQuestion{"verdict": {
		Type:         "choice",
		Instructions: "Which review verdict should this pull request get, given the review's findings and the reviewer's notes?",
		Criteria: map[string]string{
			"approve":         "ready to merge as it is",
			"comment":         "feedback that neither approves nor blocks",
			"request_changes": "must change before it can merge",
		},
	}}
)

type findingState struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Finding string `json:"finding"`
	Hunk    string `json:"hunk,omitempty"`
}

type verdictFinding struct {
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Finding  string `json:"finding"`
}

type verdictState struct {
	Title     string           `json:"pr_title"`
	Body      string           `json:"pr_body,omitempty"`
	Findings  []verdictFinding `json:"findings"`
	Rationale string           `json:"rationale,omitempty"`
}

// splitLabel returns a finding's Conventional-Comments label ("" if none) and
// its text without it, so the label (the baseline) never reaches the state.
func splitLabel(body string) (label, text string) {
	text = carriedOverRe.ReplaceAllString(strings.TrimSpace(body), "")
	first, _, _ := strings.Cut(text, "\n")
	m := reviewLabelRe.FindStringSubmatchIndex(first)
	if m == nil {
		return "", text
	}
	return strings.ToLower(text[m[2]:m[3]]), strings.TrimSpace(text[m[1]:])
}

// hunkAt returns the @@ hunk of patch whose new side covers line, or "".
func hunkAt(patch string, line int) string {
	for i, h := range strings.Split(patch, "\n@@") {
		if i > 0 {
			h = "@@" + h
		}
		if parsePatch(h).right[line] {
			return h
		}
	}
	return ""
}

// observeReview asks the review's shadow questions off the posting path, so a
// slow or failing handler can never delay or fail the review.
func (a *App) observeReview(ctx context.Context, owner, repo string, number int, verdict, body string, comments []sdk.ReviewComment) {
	if a.decide == nil {
		return
	}
	go a.decideReview(context.WithoutCancel(ctx), owner, repo, number, verdict, body, comments)
}

// decideReview runs the calls one at a time: a burst per finding can exhaust the handler's GPU batch.
func (a *App) decideReview(ctx context.Context, owner, repo string, number int, verdict, body string, comments []sdk.ReviewComment) {
	pctx, cancel := context.WithTimeout(ctx, decideTimeout)
	positions, _ := a.commentablePositions(pctx, owner, repo, number)
	cancel()
	seen := map[sdk.ReviewComment]bool{}
	var findings []verdictFinding
	for _, c := range comments {
		if seen[c] {
			continue
		}
		seen[c] = true
		label, text := splitLabel(c.Body)
		// The host's classification wins; the body label is the fallback for hand-written comments.
		if sev := strings.ToLower(strings.TrimSpace(c.Severity)); severityQuestions["severity"].Criteria.(map[string]string)[sev] != "" {
			label = sev
		}
		st := findingState{Path: c.Path, Line: c.Line, Finding: truncate(text, findingCap)}
		if p, err := resolvePath(positions, c.Path); err == nil {
			st.Hunk = truncate(hunkAt(positions[p].patch, c.Line), hunkCap)
		}
		if label != "" {
			a.ask(ctx, sdk.DecideRequest{Point: "finding.severity", State: st, Questions: severityQuestions,
				Primary: "severity", Restrictive: []string{"blocking"}, Baseline: label})
		}
		a.ask(ctx, sdk.DecideRequest{Point: "finding.blocking", State: st, Questions: blockingQuestions,
			Primary: "blocking", Restrictive: []string{"true"}, Baseline: strconv.FormatBool(label == "blocking")})
		findings = append(findings, verdictFinding{Severity: label, Path: c.Path, Line: c.Line, Finding: text})
	}
	verdict = strings.ToLower(strings.TrimSpace(verdict))
	if !reviewEvents[strings.ToUpper(verdict)] {
		return
	}
	per := max(200, findingsBudget/max(1, len(findings)))
	for i := range findings {
		findings[i].Finding = truncate(findings[i].Finding, per)
	}
	st := verdictState{Findings: findings, Rationale: truncate(verdictLineRe.ReplaceAllString(StripVerdictTail(body), ""), rationaleCap)}
	mctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	if m, err := a.pullMeta(mctx, owner, repo, number); err == nil {
		st.Title, st.Body = m.Title, truncate(m.Body, prBodyCap)
	}
	a.ask(ctx, sdk.DecideRequest{Point: "review.verdict", State: st, Questions: verdictQuestions,
		Primary: "verdict", Restrictive: []string{"comment", "request_changes"}, Baseline: verdict})
}

func (a *App) ask(ctx context.Context, req sdk.DecideRequest) {
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	if _, err := a.decide(ctx, req); err != nil {
		slog.Debug("github: no decision", "component", "github", "point", req.Point, "err", err)
	}
}
