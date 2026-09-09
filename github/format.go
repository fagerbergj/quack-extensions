package github

import (
	"regexp"
	"strings"
)

// mentionRe matches a GitHub @user or @org/team token; the leading class
// keeps emails (a@b) and URL paths (/@x) from matching.
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
		// Even segments are prose, odd ones are inline code.
		parts := strings.Split(ln, "`")
		for j := 0; j < len(parts); j += 2 {
			parts[j] = mentionRe.ReplaceAllString(parts[j], "${1}${2}")
		}
		lines[i] = strings.Join(parts, "`")
	}
	return strings.Join(lines, "\n")
}

// appendLine adds line to body once: the same outcome re-evaluated on a later
// event (another check completing, another labeled delivery) must not stack.
func appendLine(body, line string) (string, bool) {
	if strings.Contains(body, line) {
		return body, false
	}
	if body = strings.TrimRight(body, "\n"); body == "" {
		return line, true
	}
	return body + "\n\n" + line, true
}
