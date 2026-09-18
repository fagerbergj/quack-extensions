package sleeper

import (
	"context"
	"testing"
)

func TestGetPlayerByID(t *testing.T) {
	e := testExtension(t)
	got, err := e.getPlayer(context.Background(), playerArgs{PlayerID: "6797"})
	if err != nil {
		t.Fatalf("getPlayer: %v", err)
	}
	if got.Name != "Justin Herbert" {
		t.Errorf("name = %q, want Justin Herbert", got.Name)
	}
	if got.Position != "QB" || got.Team != "LAC" {
		t.Errorf("position/team = %q/%q, want QB/LAC", got.Position, got.Team)
	}
	if got.FetchedAt == "" {
		t.Error("expected fetched_at to be set")
	}
}

func TestGetPlayerByName(t *testing.T) {
	e := testExtension(t)
	got, err := e.getPlayer(context.Background(), playerArgs{Name: "Buccaneers"})
	if err != nil {
		t.Fatalf("getPlayer: %v", err)
	}
	if got.PlayerID != "TB" {
		t.Errorf("player_id = %q, want TB", got.PlayerID)
	}
}

func TestGetPlayerRequiresIDOrName(t *testing.T) {
	e := testExtension(t)
	if _, err := e.getPlayer(context.Background(), playerArgs{}); err == nil {
		t.Fatal("expected an error when neither player_id nor name is given")
	}
}
