package sleeper

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
)

func TestFactoryRejectsUnknownKey(t *testing.T) {
	_, err := factory(sdk.Host{}, []byte("default_user: jf\nbogus_key: 1\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown config key, got nil")
	}
}

func TestFactorySnapshotsValidation(t *testing.T) {
	for _, tc := range []struct {
		snapshots string
		wantErr   bool
	}{
		{"", false},
		{"off", false},
		{"daily", false},
		{"weekly", true},
	} {
		raw := []byte("snapshots: " + tc.snapshots + "\n")
		if tc.snapshots == "" {
			raw = nil
		}
		_, err := factory(sdk.Host{}, raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("snapshots=%q: err=%v, wantErr=%v", tc.snapshots, err, tc.wantErr)
		}
	}
}

func TestFactorySeasonValidation(t *testing.T) {
	if _, err := factory(sdk.Host{}, []byte("season: -1\n")); err == nil {
		t.Error("expected a negative season to error")
	} else if !strings.Contains(err.Error(), "season") {
		t.Errorf("error %q should name the season field", err)
	}
	if _, err := factory(sdk.Host{}, []byte("season: 2026\n")); err != nil {
		t.Errorf("season: 2026 should be valid, got %v", err)
	}
	if _, err := factory(sdk.Host{}, nil); err != nil {
		t.Errorf("no config (season defaults to /v1/state/nfl) should be valid, got %v", err)
	}
}

// TestFactoryIsSideEffectFree calls factory with a zero-value Host, whose
// function fields (Dispatch, Log, ...) are all nil - factory must not
// invoke any of them, only validate config and construct.
func TestFactoryIsSideEffectFree(t *testing.T) {
	ext, err := factory(sdk.Host{}, []byte("default_user: jf\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if ext.Tools() != nil {
		t.Errorf("Tools() = %v, want nil (no tools in this slice)", ext.Tools())
	}
}
