package mutator

import (
	"context"
	"fmt"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

type UpdateCounterAction struct {
	Name      string
	Capacity  *int64
	Count     *int64
	CountDiff int64
	Recorder  record.EventRecorder
	Result    chan error
}

func (a *UpdateCounterAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	counter, ok := gs.Status.Counters[a.Name]
	if !ok {
		return nil // or error if strict
	}
	if a.Capacity != nil {
		counter.Capacity = *a.Capacity
	}
	if a.Count != nil {
		counter.Count = *a.Count
	}
	if a.CountDiff != 0 {
		newCnt := counter.Count + a.CountDiff
		if newCnt < 0 {
			newCnt = 0
		}
		if newCnt > counter.Capacity {
			newCnt = counter.Capacity
		}
		counter.Count = newCnt
	}
	gs.Status.Counters[a.Name] = counter
	return nil
}

func (a *UpdateCounterAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	if a.Recorder != nil {
		c := gs.Status.Counters[a.Name]
		a.Recorder.Event(gs, corev1.EventTypeNormal, "UpdateCounter",
			fmt.Sprintf("Counter %s updated to Count:%d Capacity:%d", a.Name, c.Count, c.Capacity))
	}
	return nil
}

func (a *UpdateCounterAction) ErrCh() chan error { return a.Result }
