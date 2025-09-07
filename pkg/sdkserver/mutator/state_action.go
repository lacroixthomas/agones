package mutator

import (
	"context"
	"fmt"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/gameserverallocations"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

type ReserveTimerFunc func(duration time.Duration)

type SetStateAction struct {
	NewState        agonesv1.GameServerState
	ReserveDuration *time.Duration
	Recorder        record.EventRecorder
	ReserveTimer    ReserveTimerFunc // Injected callback to start reserve timer
	Result          chan error
}

func (a *SetStateAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	gs.Status.State = a.NewState

	if a.NewState == agonesv1.GameServerStateReserved && a.ReserveDuration != nil {
		n := metav1.NewTime(time.Now().Add(*a.ReserveDuration))
		gs.Status.ReservedUntil = &n
	} else {
		gs.Status.ReservedUntil = nil
	}

	if a.NewState == agonesv1.GameServerStateAllocated {
		ts, err := time.Now().MarshalText()
		if err == nil {
			if gs.ObjectMeta.Annotations == nil {
				gs.ObjectMeta.Annotations = map[string]string{}
			}
			gs.ObjectMeta.Annotations[gameserverallocations.LastAllocatedAnnotationKey] = string(ts)
		}
	}
	return nil
}

func (a *SetStateAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	message := "SDK state change"
	level := corev1.EventTypeNormal

	switch gs.Status.State {
	case agonesv1.GameServerStateUnhealthy:
		level = corev1.EventTypeWarning
		message = "Health check failure"
	case agonesv1.GameServerStateReserved:
		if a.ReserveDuration != nil {
			message += fmt.Sprintf(", for %s", a.ReserveDuration)
			if a.ReserveTimer != nil {
				a.ReserveTimer(*a.ReserveDuration)
			}
		}
	}

	if a.Recorder != nil {
		a.Recorder.Event(gs, level, string(gs.Status.State), message)
	}
	return nil
}

func (a *SetStateAction) ErrCh() chan error { return a.Result }

func toMetaV1TimePtr(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}
