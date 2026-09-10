package github

import (
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

// footerRe matches quack's own trailing <sub>...</sub> footer block -
// always the body's last two-newline-separated block - so appendLine can
// slot a new line above it instead of after it.
var footerRe = regexp.MustCompile(`\n\n<sub>quack[^\n]*</sub>\s*\z`)

// withFooter appends quack's version/run-link footer after one blank line,
// once: the owner's two asks were "show the quack version somewhere
// inconspicuous" and "link a posted review/comment back to the run that
// produced it". body is unchanged when the host set neither Host.Version
// nor Host.PublicURL, or when body already carries this exact footer
// (idempotent re-render, e.g. a revised-then-reposted comment).
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
