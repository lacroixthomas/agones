package mutator

import (
	"context"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

type PlayerDisconnectAction struct {
	PlayerID string
	Result   chan error
}

func (a *PlayerDisconnectAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.Status.Players == nil {
		return nil
	}
	newIDs := make([]string, 0, len(gs.Status.Players.IDs))
	for _, id := range gs.Status.Players.IDs {
		if id != a.PlayerID {
			newIDs = append(newIDs, id)
		}
	}
	gs.Status.Players.IDs = newIDs
	return nil
}

func (a *PlayerDisconnectAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	return nil
}

func (a *PlayerDisconnectAction) ErrCh() chan error { return a.Result }
