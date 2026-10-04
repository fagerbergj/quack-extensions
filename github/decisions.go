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

// decideTimeout bounds each Decide call and each GitHub read the state needs.
var decideTimeout = 10 * time.Second

// Caps keep a state well under the handler's 8192-token input cap.
const (
	hunkCap        = 4000
	findingCap     = 4000
	prBodyCap      = 2000
	rationaleCap   = 6000
	findingsBudget = 12000
	// The intent and ci.flaky states stay under ~3.5k tokens for a 4096-token handler.
	commentCap = 6000
	titleCap   = 300
	filesCap   = 50
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
	intentQuestions = map[string]sdk.DecisionQuestion{
		"write": {Type: "noul",
			Instructions: "Does this message ask quack to change code, push, merge, label, or otherwise write to the repository?"},
		"deliverable": {Type: "choice",
			Instructions: "What should quack produce in answer to this message?",
			Criteria: map[string]string{
				"reply":        "a comment answering the message, with no new work",
				"review":       "a code review of the pull request",
				"commit":       "a commit pushed to this pull request",
				"pull_request": "code written and a new pull request opened",
				"plan":         "an implementation plan posted as a comment",
			}},
	}
	flakyQuestions = map[string]sdk.DecisionQuestion{"flaky": {
		Type:         "noul",
		Instructions: "Is this CI failure unrelated to the PR's changes (a flaky or infrastructure failure)?",
	}}
)

var decisionPoints = []sdk.DecisionPoint{
	{Name: "finding.severity", Description: "the Conventional Comments label of one review finding",
		Questions: severityQuestions, Primary: "severity", Restrictive: []string{"blocking"}, Modes: []string{"observe"}},
	{Name: "finding.blocking", Description: "whether one review finding should block the merge",
		Questions: blockingQuestions, Primary: "blocking", Restrictive: []string{"true"}, Modes: []string{"observe"}},
	{Name: "review.verdict", Description: "the verdict of a whole review",
		Questions: verdictQuestions, Primary: "verdict", Restrictive: []string{"comment", "request_changes"}, Modes: []string{"observe"}},
	{Name: "intent", Description: "whether a comment asks quack to write to the repository, and which deliverable it asks for",
		Questions: intentQuestions, Primary: "write", Restrictive: []string{"false"}, Modes: []string{"observe"}},
	{Name: "ci.flaky", Description: "whether one failed check on a PR quack is fixing is unrelated to the PR's changes",
		Questions: flakyQuestions, Primary: "flaky", Modes: []string{"observe"}},
}

// DecisionPoints declares the review, intent and CI points, observe only: the host records the
// answers and they never change what this extension posts.
func (e *Extension) DecisionPoints() []sdk.DecisionPoint { return decisionPoints }

type intentState struct {
	Subject     string `json:"subject"` // "issue" or "pull request"
	Title       string `json:"title"`
	Sender      string `json:"sender"`
	Association string `json:"author_association,omitempty"`
	Grant       string `json:"grant"` // the delivery kinds the labels and authorship allow
	Comment     string `json:"comment"`
}

type ciFailureState struct {
	Repo         string   `json:"repo"`
	PR           int      `json:"pr"`
	HeadSHA      string   `json:"head_sha"`
	Check        string   `json:"check"`
	Failure      string   `json:"failure"`
	ChangedFiles []string `json:"changed_files"`
}

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
			a.ask(ctx, sdk.DecideRequest{Point: "finding.severity", State: st, Baseline: label})
		}
		a.ask(ctx, sdk.DecideRequest{Point: "finding.blocking", State: st, Baseline: strconv.FormatBool(label == "blocking")})
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
	a.ask(ctx, sdk.DecideRequest{Point: "review.verdict", State: st, Baseline: verdict})
}

// observeIntent asks the intent point for a free-text comment trigger, off the
// dispatch path; kind is "" for synthetic and command triggers, which nothing classifies.
func (e *Extension) observeIntent(ctx context.Context, p issueCommentPayload, chatID string, allowedKinds []string, kind string) {
	if e.app.decide == nil || kind == "" || p.isLabelTrigger {
		return
	}
	subject := "issue"
	if p.Issue.PullRequest != nil {
		subject = "pull request"
	}
	st := intentState{Subject: subject, Title: truncate(p.Issue.Title, titleCap), Sender: p.Comment.User.Login,
		Association: p.Comment.AuthorAssociation, Grant: permissionsText(allowedKinds), Comment: truncate(p.Comment.Body, commentCap)}
	write := kind == "commit" || kind == "pull_request"
	go e.app.ask(context.WithoutCancel(ctx), sdk.DecideRequest{Point: "intent", State: st, Baseline: strconv.FormatBool(write), ChatID: chatID})
}

// observeCIFailure asks ci.flaky once per failing check, off the fix path. The
// baseline is empty: a scorer joins (head_sha, check) to the check's later conclusion.
func (e *Extension) observeCIFailure(ctx context.Context, chatID string, ri repoInfo, number int, sha string, checks []failingCheck) {
	if e.app.decide == nil || len(checks) == 0 {
		return
	}
	go e.app.decideCIFailure(context.WithoutCancel(ctx), chatID, ri.Owner, ri.Name, number, sha, checks)
}

func (a *App) decideCIFailure(ctx context.Context, chatID, owner, repo string, number int, sha string, checks []failingCheck) {
	fctx, cancel := context.WithTimeout(ctx, decideTimeout)
	files, _ := a.pullFiles(fctx, owner, repo, number)
	cancel()
	names := []string{}
	for _, f := range files[:min(len(files), filesCap)] {
		names = append(names, f.Filename)
	}
	for _, c := range checks[:min(len(checks), maxFailingChecks)] {
		st := ciFailureState{Repo: owner + "/" + repo, PR: number, HeadSHA: sha, Check: c.Name,
			Failure: truncate(renderOneCheck(c), maxChecksContextRunes), ChangedFiles: names}
		a.ask(ctx, sdk.DecideRequest{Point: "ci.flaky", State: st, ChatID: chatID})
	}
}

func (a *App) ask(ctx context.Context, req sdk.DecideRequest) {
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	if _, err := a.decide(ctx, req); err != nil {
		slog.Debug("github: no decision", "component", "github", "point", req.Point, "err", err)
	}
}
