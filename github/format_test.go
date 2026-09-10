package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFooterTextVariants(t *testing.T) {
	tests := []struct {
		name               string
		version, publicURL string
		want               string
	}{
		{"both", "0.51.26", "https://quack.example.com", `<sub>quack 0.51.26 · <a href="https://quack.example.com/chat/chat1">run</a></sub>`},
		{"only version", "0.51.26", "", "<sub>quack 0.51.26</sub>"},
		{"only URL", "", "https://quack.example.com", `<sub>quack · <a href="https://quack.example.com/chat/chat1">run</a></sub>`},
		{"neither", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := footerText(tt.version, tt.publicURL, "chat1"); got != tt.want {
				t.Errorf("footerText(%q, %q, chat1) = %q, want %q", tt.version, tt.publicURL, got, tt.want)
			}
		})
	}
}

func TestWithFooterNeitherFieldSetLeavesBodyUnchanged(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if got := app.withFooter("LGTM", "chat1"); got != "LGTM" {
		t.Errorf("withFooter with no version/URL = %q, want body unchanged", got)
	}
}

func TestWithFooterIdempotent(t *testing.T) {
	app, err := NewApp("1", mustTestKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.SetFooter("0.51.26", "https://quack.example.com")
	once := app.withFooter("LGTM", "chat1")
	if !strings.Contains(once, "<sub>quack 0.51.26") {
		t.Fatalf("first render has no footer: %q", once)
	}
	twice := app.withFooter(once, "chat1")
	if twice != once {
		t.Errorf("re-rendering an already-footed body changed it: got %q, want %q", twice, once)
	}
	if n := strings.Count(twice, "<sub>quack"); n != 1 {
		t.Errorf("footer appears %d times, want 1: %q", n, twice)
	}
}

// TestAppendLineAfterFooter pins merge.go's appendToVerdict expectation: the
// footer must stay the body's last line, with the outcome line slotted above
// it - not appended after, which would bury the footer mid-body.
func TestAppendLineAfterFooter(t *testing.T) {
	withFooter := "LGTM\n\n<!-- quack:delivery:review:approve -->\n\n<sub>quack 0.51.26 · <a href=\"https://quack.example.com/chat/chat1\">run</a></sub>"
	got, changed := appendLine(withFooter, "Merged as abc1234.")
	if !changed {
		t.Fatal("expected the outcome line to be added")
	}
	want := "LGTM\n\n<!-- quack:delivery:review:approve -->\n\nMerged as abc1234.\n\n<sub>quack 0.51.26 · <a href=\"https://quack.example.com/chat/chat1\">run</a></sub>"
	if got != want {
		t.Errorf("appendLine after footer = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "</sub>") {
		t.Errorf("footer no longer last: %q", got)
	}
	// Idempotent even with the footer present.
	if again, changed := appendLine(got, "Merged as abc1234."); changed || again != got {
		t.Errorf("re-appending the same outcome line changed the body: %q, %v", again, changed)
	}
}

// TestReviewVerdictMarkerFoundWithFooterPresent pins that the own-PR verdict
// lookup (merge.go's reviewVerdictMarkerRe/reviewHeadMarkerRe) still finds
// its markers once withFooter's trailing <sub> block sits after them.
func TestReviewVerdictMarkerFoundWithFooterPresent(t *testing.T) {
	body := "_Own PR: verdict approve._\n\n<!-- quack:delivery:review:approve -->\n<!-- quack:delivery:head:deadbeef -->\n\n<sub>quack 0.51.26 · <a href=\"https://quack.example.com/chat/chat1\">run</a></sub>"
	m := reviewVerdictMarkerRe.FindStringSubmatch(body)
	if m == nil || m[1] != "approve" {
		t.Fatalf("reviewVerdictMarkerRe found %v, want [approve]", m)
	}
	hm := reviewHeadMarkerRe.FindStringSubmatch(body)
	if hm == nil || hm[1] != "deadbeef" {
		t.Fatalf("reviewHeadMarkerRe found %v, want [deadbeef]", hm)
	}
}

// TestSubmitReviewCarriesMarkerAndFooter is an end-to-end check that a
// posted review body keeps the delivery marker findable (deliverStagedComment/
// collapsePriorReviews-style Contains lookups) alongside the trailing footer.
func TestSubmitReviewCarriesMarkerAndFooter(t *testing.T) {
	var posted string
	app := newReviewApp(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(b, &body)
		posted = body.Body
		io.WriteString(w, `{"id":1,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-1"}`)
	})
	app.SetFooter("0.51.26", "https://quack.example.com")
	if _, err := app.submitReview(context.Background(), submitReviewArgs{
		Owner: "acme", Repo: "widgets", PullNumber: 7, Body: "LGTM", Event: "APPROVE", ChatID: "chat1",
	}); err != nil {
		t.Fatalf("submitReview: %v", err)
	}
	if !strings.Contains(posted, deliveryMarker("review")) {
		t.Errorf("posted review lost its delivery marker: %q", posted)
	}
	want := `<sub>quack 0.51.26 · <a href="https://quack.example.com/chat/chat1">run</a></sub>`
	if !strings.HasSuffix(strings.TrimRight(posted, "\n"), want) {
		t.Errorf("posted review does not end with the footer: %q", posted)
	}
}

func mustTestKeyPEM(t *testing.T) string {
	t.Helper()
	pem, _ := testKeyPEM(t)
	return pem
}
