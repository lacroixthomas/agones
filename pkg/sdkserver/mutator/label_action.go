package mutator

import (
	"context"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

type SetLabelAction struct {
	Key    string
	Value  string
	Result chan error
}

func (a *SetLabelAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.ObjectMeta.Labels == nil {
		gs.ObjectMeta.Labels = map[string]string{}
	}
	gs.ObjectMeta.Labels["agones.dev/sdk-"+a.Key] = a.Value
	return nil
}

func (a *SetLabelAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	return nil
}

func (a *SetLabelAction) ErrCh() chan error { return a.Result }
