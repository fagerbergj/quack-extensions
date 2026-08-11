package github

import (
	"regexp"
	"strings"
)

// Duplicated from internal/vetting/answerreview.go's StripVerdictTail (used
// there only by tools.go's self-review path, which now lives here) - not
// deleted from vetting, since its verdictRe/fallbackPreambleRe regexes are
// shared with parseAnswerReview, which stays quack-core (a generic ACP
// reviewer-answer probe, not GitHub-specific). vetting.StripVerdictTail
// itself has no remaining caller after this move and is a candidate for a
// follow-up cleanup, out of scope here to keep quack-core untouched.
var (
	verdictRe          = regexp.MustCompile(`(?mi)^\s*VERDICT:\s*(approve|request_changes|comment)\s*$`)
	fallbackPreambleRe = regexp.MustCompile(`(?mi)^.*\bstaging tools?\b.*\bfallback\b.*$\n?`)
)

// StripVerdictTail removes the machine-parseable VERDICT tail for human-facing text.
func StripVerdictTail(answer string) string {
	s := fallbackPreambleRe.ReplaceAllString(answer, "")
	if loc := verdictRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	return strings.TrimSpace(s)
}
