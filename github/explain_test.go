package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const explainChatID = "ext:github:github-acme-widgets-7-explain-alice"

func TestHandleWebhookExplainCommand(t *testing.T) {
	plainIssue := []byte(strings.Replace(string(reviewCommandBody("/explain", "OWNER")), `,"pull_request":{}`, "", 1))
	tests := []struct {
		name     string
		triggers []string
		body     []byte
		wantRun  bool
	}{
		{"bare /explain from a collaborator fires, no label needed", []string{"explain"}, reviewCommandBody(" /explain\n", "COLLABORATOR"), true},
		{"read-only author is a no-op", []string{"explain"}, reviewCommandBody("/explain", "CONTRIBUTOR"), false},
		{"extra text is not the command", []string{"explain"}, reviewCommandBody("/explain please", "OWNER"), false},
		{"explain trigger disabled is a no-op", []string{"mention", "label"}, reviewCommandBody("/explain", "OWNER", "quack-auto-review"), false},
		{"a plain issue is a no-op", []string{"explain"}, plainIssue, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 4)
			srv := stubGitHub(t, posted)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)
			ext.app.publicURL = "https://quack.example"

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", tt.body))
			if !tt.wantRun {
				select {
				case <-fh.notify:
					t.Error("should not have dispatched a run")
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d; want 202", rec.Code)
			}
			req := fh.waitForDispatch(t, 2*time.Second)
			if req.Chat.LocalID != "github-acme-widgets-7-explain-alice" {
				t.Errorf("LocalID = %q; want a per-user explain session apart from the PR's review session", req.Chat.LocalID)
			}
			if !strings.HasPrefix(req.Chat.Title, "Explain:") {
				t.Errorf("Title = %q; want an Explain: prefix", req.Chat.Title)
			}
			if k := req.Delivery.AllowedKinds; k == nil || len(k) != 0 {
				t.Errorf("AllowedKinds = %#v; want non-nil empty (deny every delivery)", k)
			}
			for _, ask := range []string{req.Ask.Message, req.Ask.NodeContext} {
				if !strings.Contains(ask, "<deliverable>Walk me through and quiz me on PR acme/widgets#7:") {
					t.Errorf("ask = %q; want the walkthrough-and-quiz deliverable", ask)
				}
			}
			select {
			case body := <-posted:
				if !strings.Contains(body, "https://quack.example/chat/"+explainChatID) {
					t.Errorf("link comment = %q; want the chat URL", body)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no link comment posted")
			}
		})
	}
}

func TestHandleWebhookExplainCommandRespectsAllowlist(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"explain"})
	ext.allowedUsers = map[string]bool{"someone-else": true}

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody("/explain", "OWNER")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	select {
	case <-fh.notify:
		t.Error("disallowed user should not dispatch")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestExplainRunEndedPostsNothing covers the restart path too: the explain
// flag is rebuilt from the durable row's session id, not lost.
func TestExplainRunEndedPostsNothing(t *testing.T) {
	posted := make(chan string, 4)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"explain"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody("/explain", "OWNER")))
	fh.waitForDispatch(t, 2*time.Second)
	select {
	case body := <-posted:
		if !strings.Contains(body, "chat `"+explainChatID+"`") {
			t.Errorf("link comment = %q; want the chat id when public_url is unset", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no link comment posted")
	}

	ext.pending.Delete(explainChatID) // as after a restart: only the durable row remains
	ext.RunEnded(explainChatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "walkthrough rendered"})
	select {
	case body := <-posted:
		t.Errorf("explain run posted %q; want nothing", body)
	case <-time.After(200 * time.Millisecond):
	}
	if len(fh.calls()) != 1 {
		t.Errorf("dispatches = %d; want 1 (no nudge)", len(fh.calls()))
	}
}
