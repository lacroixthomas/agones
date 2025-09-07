package mutator

import (
	"context"
	"fmt"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

type SetPlayerCapacityAction struct {
	NewCapacity int64
	Recorder    record.EventRecorder
	Result      chan error
}

func (a *SetPlayerCapacityAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.Status.Players == nil {
		gs.Status.Players = &agonesv1.PlayerStatus{}
	}
	gs.Status.Players.Capacity = a.NewCapacity
	return nil
}

func (a *SetPlayerCapacityAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	if a.Recorder != nil {
		a.Recorder.Event(gs, corev1.EventTypeNormal, "PlayerCapacity", fmt.Sprintf("Set to %d", gs.Status.Players.Capacity))
	}
	return nil
}

func (a *SetPlayerCapacityAction) ErrCh() chan error { return a.Result }
