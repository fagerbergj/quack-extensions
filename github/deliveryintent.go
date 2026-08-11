package github

import "regexp"

// Duplicated from internal/vetting/delivery.go's ImplementationIntent:
// vetting's own gate (demandedDelivery) calls it internally too, so
// deleting the original would break the gate - only this extension's copy
// (envelope.go/intent.go's classification, tools.go's self-review path via
// StripVerdictTail below) crosses the seam.
var (
	implVerbs  = `add|implement|create|write|fix|refactor|build|port|migrate|scaffold|generate`
	implVerbRe = regexp.MustCompile(`(?i)\b(` + implVerbs + `)\b`)
	deliveryRe = regexp.MustCompile(`(?i)(pull[ -]?request|\bpr\b|\bcommit\b|\bpush\b|\bbranch\b|\bmerge\b)`)

	// identRe: matches URLs/paths/identifiers (verbs inside names are not instructions).
	identRe = regexp.MustCompile(`\S*[-_/]\S*`)

	// Directed verb: opens a sentence or follows and/then/also/please.
	clauseStart    = `(?i)(?:^|[.;:!?\n]\s*|\b(?:and|then|also|please)\s+)`
	implDirectedRe = regexp.MustCompile(clauseStart + `(?:` + implVerbs + `)\b`)

	// review/audit ask is read-only by default.
	reviewRe = regexp.MustCompile(`(?i)\b(review|audit|critique|assess)(s|ed|ing)?\b`)
)

// ImplementationIntent reports if text asks for code AND delivery (identifiers/URLs excluded).
func ImplementationIntent(text string) bool {
	if !deliveryRe.MatchString(text) {
		return false
	}
	prose := identRe.ReplaceAllString(text, " ")
	if !implVerbRe.MatchString(prose) {
		return false
	}
	// review/audit is read-only unless it also directs a change.
	return !reviewRe.MatchString(prose) || implDirectedRe.MatchString(prose)
}
