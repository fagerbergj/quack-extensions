package sleeper

import (
	"context"
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

// wantToolCount is every sleeper_* tool this slice registers.
const wantToolCount = 12

// TestFactoryIsSideEffectFree calls factory with a zero-value Host - factory
// (and building the tool list) must never invoke any of its nil fields.
func TestFactoryIsSideEffectFree(t *testing.T) {
	ext, err := factory(sdk.Host{}, []byte("default_user: jf\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if got := len(ext.Tools()); got != wantToolCount {
		t.Errorf("Tools() returned %d tools, want %d", got, wantToolCount)
	}
}

// TestStartNoopWithoutDailySnapshots covers both guards: snapshots not
// "daily", and no default_league to snapshot.
func TestStartNoopWithoutDailySnapshots(t *testing.T) {
	for _, cfg := range []config{
		{Snapshots: "off", DefaultLeague: testLeague},
		{Snapshots: "daily", DefaultLeague: ""},
	} {
		e := &extension{host: sdk.Host{}, cfg: cfg}
		if err := e.Start(context.Background()); err != nil {
			t.Errorf("Start(%+v) = %v, want nil", cfg, err)
		}
	}
}
