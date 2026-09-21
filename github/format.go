package github

import (
	"fmt"
	"regexp"
	"strings"
)

// mentionRe matches a GitHub @user or @org/team token; the leading class
// keeps emails (a@b) and URL paths (/@x) from matching. The team alternative
// also matches an npm-style scoped package name (@angular/core) in prose -
// GitHub pings @org/team identically, so there is no grammar-level way to
// tell them apart; such tokens are deliberately stripped too.
var mentionRe = regexp.MustCompile(`(^|[^\w/.:@])@([A-Za-z0-9][A-Za-z0-9-]*(?:/[A-Za-z0-9._-]+)?)`)

// stripMentions drops the "@" from every mention outside fenced or inline
// code, so nothing quack posts - its own bookkeeping or any agent's prose -
// can ping a person. The login itself stays, so the sentence still reads.
func stripMentions(s string) string {
	lines := strings.Split(s, "\n")
	// An odd number of fence markers means an unterminated block (a truncated
	// diff quote, say) - tracking fenced state would then hide every mention
	// past it for the rest of the body. Fail safe: strip everywhere instead.
	fenceCount := 0
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenceCount++
		}
	}
	trackFences := fenceCount%2 == 0
	fenced := false
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if trackFences {
				fenced = !fenced
			}
			continue
		}
		if fenced {
			continue
		}
		// Even segments are prose, odd ones are inline code. With an odd
		// backtick count the final segment is an unterminated span and the
		// parity flips, so a trailing mention would survive; fail safe like
		// the fence block above and strip it too.
		parts := strings.Split(ln, "`")
		allProse := len(parts)%2 == 0
		for j := 0; j < len(parts); j++ {
			if !allProse && j%2 != 0 {
				continue
			}
			parts[j] = mentionRe.ReplaceAllString(parts[j], "${1}${2}")
		}
		lines[i] = strings.Join(parts, "`")
	}
	return strings.Join(lines, "\n")
}

// appendLine adds line to body once: the same outcome re-evaluated on a later
// event (another check completing, another labeled delivery) must not stack.
// A trailing quack footer (see withFooter) stays last - line slots in above it.
func appendLine(body, line string) (string, bool) {
	if strings.Contains(body, line) {
		return body, false
	}
	if loc := footerRe.FindStringIndex(body); loc != nil {
		return body[:loc[0]] + "\n\n" + line + body[loc[0]:], true
	}
	if body = strings.TrimRight(body, "\n"); body == "" {
		return line, true
	}
	return body + "\n\n" + line, true
}

// footerRe matches the whole trailing footer region (commands block plus
// <sub> line, or the line alone) so appendLine slots new lines above it.
var footerRe = regexp.MustCompile(`(?s)\n\n(?:<details>\n<summary>[^\n]*</summary>\n\n.*?\n\n</details>(?:\n\n<sub>quack[^\n]*</sub>)?|<sub>quack[^\n]*</sub>)\s*\z`)

// withFooter appends the version/run-link footer once; body is unchanged
// when the host set neither field or already carries this exact footer.
func (a *App) withFooter(body, chatID string) string {
	text := footerText(a.version, a.publicURL, chatID)
	if text == "" || strings.Contains(body, text) {
		return body
	}
	if b := strings.TrimRight(body, "\n"); b != "" {
		return b + "\n\n" + text
	}
	return text
}

// footerText renders the <sub> footer itself, or "" when there is nothing to
// show - never stamp a bare "quack" with no version and no link.
func footerText(version, publicURL, chatID string) string {
	if version == "" && publicURL == "" {
		return ""
	}
	s := "<sub>quack"
	if version != "" {
		s += " " + version
	}
	if publicURL != "" {
		s += ` · <a href="` + publicURL + "/chat/" + chatID + `">run</a>`
	}
	return s + "</sub>"
}

// commandsBlockText renders the collapsible block a posted review carries,
// naming only what this deployment actually accepts - "" when every trigger
// it could name is disabled.
func commandsBlockText(mention string, labels Labels, triggers map[string]bool) string {
	var lines []string
	if triggers["mention"] && mention != "" {
		lines = append(lines,
			fmt.Sprintf("- `%s <request>` at the start of a line to continue the conversation; `%s` alone does nothing.", mention, mention))
	}
	if triggers["label"] {
		if labels.Review != "" {
			lines = append(lines, fmt.Sprintf("- `/review` as the entire comment (nothing else in it) re-runs this review while the `%s` label is on the pull request.", labels.Review))
			lines = append(lines, fmt.Sprintf("- the `%s` label: runs a review.", labels.Review))
		}
	}
	if triggers["merge"] && labels.Merge != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label: merges once quack approves and checks are green.", labels.Merge))
	}
	if triggers["ci_fix"] && labels.Fix != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label: fixes red CI.", labels.Fix))
	}
	if triggers["issue_plan"] && labels.Plan != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label on an issue: drafts a plan.", labels.Plan))
	}
	if triggers["issue_implement"] && labels.Implement != "" {
		lines = append(lines, fmt.Sprintf("- the `%s` label on an issue: implements a plan.", labels.Implement))
	}
	if len(lines) == 0 {
		return ""
	}
	return "<details>\n<summary>Commands quack accepts here</summary>\n\n" +
		strings.Join(lines, "\n") + "\n\n</details>"
}

// reviewFooterText is footerText plus the commands block, block first - the
// pair a posted review carries, either half optional.
func reviewFooterText(mention string, labels Labels, triggers map[string]bool, version, publicURL, chatID string) string {
	block := commandsBlockText(mention, labels, triggers)
	footer := footerText(version, publicURL, chatID)
	switch {
	case block == "":
		return footer
	case footer == "":
		return block
	default:
		return block + "\n\n" + footer
	}
}

// withReviewFooter is withFooter for a posted review: same idempotent
// append, but the commands block (see commandsBlockText) sits above the
// <sub> line.
func (a *App) withReviewFooter(body, chatID string) string {
	text := reviewFooterText(a.mention, a.labels, a.triggers, a.version, a.publicURL, chatID)
	if text == "" || strings.Contains(body, text) {
		return body
	}
	if b := strings.TrimRight(body, "\n"); b != "" {
		return b + "\n\n" + text
	}
	return text
}
