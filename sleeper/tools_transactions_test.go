package sleeper

import (
	"context"
	"testing"
)

func TestGetTransactions(t *testing.T) {
	e := testExtension(t)
	got, err := e.getTransactions(context.Background(), transactionsArgs{WeeksBack: 2})
	if err != nil {
		t.Fatalf("getTransactions: %v", err)
	}
	if len(got.Transactions) == 0 {
		t.Fatal("expected transactions across weeks 1-2")
	}
	for _, tx := range got.Transactions {
		if tx.Type == "" || tx.Status == "" {
			t.Errorf("transaction %+v missing type/status", tx)
		}
		if len(tx.Teams) == 0 {
			t.Errorf("transaction %+v has no team names resolved", tx)
		}
		for _, m := range append(append([]moveEntry{}, tx.Adds...), tx.Drops...) {
			if m.Name == "" || m.Name == m.PlayerID {
				t.Errorf("move %+v did not resolve a player name", m)
			}
		}
	}
}

func TestGetFreeAgents(t *testing.T) {
	e := testExtension(t)
	got, err := e.getFreeAgents(context.Background(), freeAgentsArgs{Position: "RB", Limit: 5})
	if err != nil {
		t.Fatalf("getFreeAgents: %v", err)
	}
	if len(got.FreeAgents) == 0 {
		t.Fatal("expected some RB free agents")
	}
	if len(got.FreeAgents) > 5 {
		t.Errorf("free_agents = %d, want <=5 (limit)", len(got.FreeAgents))
	}
	for i, fa := range got.FreeAgents {
		if fa.Position != "RB" {
			t.Errorf("free agent %+v has position %q, want RB", fa, fa.Position)
		}
		if i > 0 && fa.Projection > got.FreeAgents[i-1].Projection {
			t.Errorf("free agents not sorted by projection desc at index %d", i)
		}
	}
}
