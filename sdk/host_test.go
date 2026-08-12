package sdk_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// TestHostContextDirChatUserArchiveChat pins the v0.3.0 Host additions'
// signatures - a real caller (quack's serve package) wires these as plain
// closures, so the sdk package itself only proves the shape is callable.
func TestHostContextDirChatUserArchiveChat(t *testing.T) {
	var archived []string
	h := sdk.Host{
		EnsureContextDir: func(userID, chatID string) (string, error) {
			if userID == "" || chatID == "" {
				return "", errors.New("missing id")
			}
			return "/data/" + userID + "/" + chatID, nil
		},
		ChatUser: func(chatID string) (string, bool) {
			if chatID == "known" {
				return "alice", true
			}
			return "", false
		},
		ArchiveChat: func(chatID string) error {
			archived = append(archived, chatID)
			return nil
		},
	}

	dir, err := h.EnsureContextDir("u1", "c1")
	if err != nil || dir != "/data/u1/c1" {
		t.Errorf("EnsureContextDir(u1, c1) = (%q, %v), want (/data/u1/c1, nil)", dir, err)
	}

	if user, ok := h.ChatUser("known"); !ok || user != "alice" {
		t.Errorf("ChatUser(known) = (%q, %v), want (alice, true)", user, ok)
	}
	if _, ok := h.ChatUser("missing"); ok {
		t.Errorf("ChatUser(missing) ok = true, want false")
	}

	if err := h.ArchiveChat("c1"); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if len(archived) != 1 || archived[0] != "c1" {
		t.Errorf("archived = %v, want [c1]", archived)
	}
}

// TestHostClassifyDegradesGracefullyWhenNil pins that a nil Classify (no
// judge model configured) is a valid, expected state - callers must check
// before calling, not assume it's always wired.
func TestHostClassifyDegradesGracefullyWhenNil(t *testing.T) {
	var h sdk.Host
	if h.Classify != nil {
		t.Fatalf("zero-value Host.Classify = non-nil, want nil")
	}

	h.Classify = func(ctx context.Context, prompt string) (string, error) {
		if prompt == "" {
			return "", errors.New("empty prompt")
		}
		return "WORK", nil
	}
	answer, err := h.Classify(context.Background(), "please review this PR")
	if err != nil || answer != "WORK" {
		t.Errorf("Classify = (%q, %v), want (WORK, nil)", answer, err)
	}
}

// TestHostUpdateChatOriginDegradesGracefullyWhenNilAndSignalsUnknownChat pins
// the shape of the badge-refresh addition: nil is a valid, expected state
// (matching every other best-effort Host call), and ErrUnknownChat is the
// documented sentinel for a localID that never reached Dispatch.
func TestHostUpdateChatOriginDegradesGracefullyWhenNilAndSignalsUnknownChat(t *testing.T) {
	var h sdk.Host
	if h.UpdateChatOrigin != nil {
		t.Fatalf("zero-value Host.UpdateChatOrigin = non-nil, want nil")
	}

	var updated []string
	h.UpdateChatOrigin = func(localID string, origin sdk.ChatOrigin) error {
		if localID == "missing" {
			return sdk.ErrUnknownChat
		}
		updated = append(updated, localID+":"+origin.Badge)
		return nil
	}

	if err := h.UpdateChatOrigin("issue-9", sdk.ChatOrigin{Extension: "github", Badge: "closed"}); err != nil {
		t.Fatalf("UpdateChatOrigin: %v", err)
	}
	if len(updated) != 1 || updated[0] != "issue-9:closed" {
		t.Errorf("updated = %v, want [issue-9:closed]", updated)
	}

	if err := h.UpdateChatOrigin("missing", sdk.ChatOrigin{}); !errors.Is(err, sdk.ErrUnknownChat) {
		t.Errorf("UpdateChatOrigin(missing) = %v, want ErrUnknownChat", err)
	}
}

// TestRunConfigTimeoutZeroMeansUnbounded pins the field's zero-value
// meaning at the type level.
func TestRunConfigTimeoutZeroMeansUnbounded(t *testing.T) {
	var rc sdk.RunConfig
	if rc.Timeout != 0 {
		t.Errorf("zero-value RunConfig.Timeout = %v, want 0 (unbounded)", rc.Timeout)
	}
	rc.Timeout = 2 * time.Hour
	if rc.Timeout != 2*time.Hour {
		t.Errorf("RunConfig.Timeout = %v, want 2h", rc.Timeout)
	}
}

// TestSetupExistingHeadRefOverridesWorkBranch pins the field's documented
// meaning (checkout an existing branch, not create WorkBranch fresh) at the
// type level - the actual override logic lives in quack, not the SDK.
func TestSetupExistingHeadRefOverridesWorkBranch(t *testing.T) {
	s := sdk.Setup{Repo: "https://github.com/acme/widgets", BaseRef: "main", WorkBranch: "quack/issue-9", ExistingHeadRef: "fix-typo"}
	if s.ExistingHeadRef != "fix-typo" {
		t.Errorf("ExistingHeadRef = %q, want fix-typo", s.ExistingHeadRef)
	}
	if s.WorkBranch != "quack/issue-9" {
		t.Errorf("WorkBranch = %q, want quack/issue-9 (ExistingHeadRef overrides at consumption time, not storage time)", s.WorkBranch)
	}
}
