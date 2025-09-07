package mutator

import (
	"context"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

type PlayerConnectAction struct {
	PlayerID string
	Result   chan error
}

func (a *PlayerConnectAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.Status.Players == nil {
		gs.Status.Players = &agonesv1.PlayerStatus{}
	}
	for _, id := range gs.Status.Players.IDs {
		if id == a.PlayerID {
			return nil
		}
	}
	gs.Status.Players.IDs = append(gs.Status.Players.IDs, a.PlayerID)
	return nil
}

func (a *PlayerConnectAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	return nil
}

func (a *PlayerConnectAction) ErrCh() chan error { return a.Result }
