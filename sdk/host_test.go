package sdk_test

import (
	"errors"
	"testing"

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
