package mutator

import (
	"context"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

type SetAnnotationAction struct {
	Key    string
	Value  string
	Result chan error
}

func (a *SetAnnotationAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.ObjectMeta.Annotations == nil {
		gs.ObjectMeta.Annotations = map[string]string{}
	}
	gs.ObjectMeta.Annotations["agones.dev/sdk-"+a.Key] = a.Value
	return nil
}

func (a *SetAnnotationAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	return nil
}

func (a *SetAnnotationAction) ErrCh() chan error { return a.Result }
