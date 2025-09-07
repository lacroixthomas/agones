package mutator

import (
	"context"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

type SetConnectedPlayersAction struct {
	PlayerIDs []string
	Result    chan error
}

func (a *SetConnectedPlayersAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.Status.Players == nil {
		gs.Status.Players = &agonesv1.PlayerStatus{}
	}
	gs.Status.Players.IDs = a.PlayerIDs
	gs.Status.Players.Count = int64(len(a.PlayerIDs))
	return nil
}

func (a *SetConnectedPlayersAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	return nil
}

func (a *SetConnectedPlayersAction) ErrCh() chan error { return a.Result }
