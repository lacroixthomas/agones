package mutator

import (
	"context"
	"fmt"
	"slices"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

type UpdateListAction struct {
	Name        string
	UpdateMask  []string
	NewCapacity int64
	NewValues   []string
	Recorder    record.EventRecorder
	Result      chan error
}

func (a *UpdateListAction) Mutate(ctx context.Context, gs *agonesv1.GameServer) error {
	defer close(a.Result)

	if gs.Status.Lists == nil {
		gs.Status.Lists = map[string]agonesv1.ListStatus{}
	}
	list, ok := gs.Status.Lists[a.Name]
	if !ok {
		return nil // or error if strict
	}

	if slices.Contains(a.UpdateMask, "capacity") {
		list.Capacity = a.NewCapacity
	}
	if slices.Contains(a.UpdateMask, "values") {
		// Remove values not in NewValues
		valuesToDelete := map[string]bool{}
		for _, value := range list.Values {
			found := false
			for _, v := range a.NewValues {
				if value == v {
					found = true
					break
				}
			}
			if !found {
				valuesToDelete[value] = true
			}
		}
		list.Values = deleteValues(list.Values, valuesToDelete)
		// Add new values not already present
		for _, value := range a.NewValues {
			found := false
			for _, v := range list.Values {
				if value == v {
					found = true
					break
				}
			}
			if !found {
				list.Values = append(list.Values, value)
			}
		}
		// Truncate if needed
		if int64(len(list.Values)) > list.Capacity {
			list.Values = append([]string{}, list.Values[:list.Capacity]...)
		}
	}
	gs.Status.Lists[a.Name] = list
	return nil
}

func (a *UpdateListAction) Finalize(ctx context.Context, gs *agonesv1.GameServer) error {
	if a.Recorder != nil {
		a.Recorder.Event(gs, corev1.EventTypeNormal, "UpdateList", fmt.Sprintf("List %s updated", a.Name))
	}
	return nil
}

func (a *UpdateListAction) ErrCh() chan error { return a.Result }

func deleteValues(valuesList []string, toDeleteValues map[string]bool) []string {
	newValuesList := []string{}
	for _, value := range valuesList {
		if _, ok := toDeleteValues[value]; ok {
			continue
		}
		newValuesList = append(newValuesList, value)
	}
	return newValuesList
}
