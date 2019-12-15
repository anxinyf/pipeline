package taskrun

import (
	"fmt"
	"knative.dev/pkg/controller"
	"time"

	apispipeline "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
)

const ControllerName = "TTLExpiredControoler"

func (tc *Reconciler) AddTaskRun(obj interface{}) {
	tr := obj.(*apispipeline.TaskRun)
	tc.Logger.Infof("Adding TaskRun %s/%s if the TaskRun has succeeded or failed and has a TTL set.", tr.Namespace, tr.Name)

	if tr.DeletionTimestamp == nil && taskRunCleanup(tr) {
		controller.NewImpl(tc, tc.Logger, ControllerName).Enqueue(tr)
	}
}

func (tc *Reconciler) UpdateTaskRun(old, cur interface{}) {
	tr := cur.(*apispipeline.TaskRun)
	tc.Logger.Infof("Updating TaskRun %s/%s if the TaskRun has succeeded or failed and has a TTL set.", tr.Namespace, tr.Name)

	if tr.DeletionTimestamp == nil && taskRunCleanup(tr) {
		controller.NewImpl(tc, tc.Logger, ControllerName).Enqueue(tr)
	}
}

// processTaskRun will check the TaskRun's state and TTL and delete the TaskRun when it
// finishes and its TTL after finished has expired. If the TaskRun hasn't finished or
// its TTL hasn't expired, it will be added to the queue after the TTL is expected
// to expire.
// This function is not meant to be invoked concurrently with the same key.
func (tc *Reconciler) processTaskRunExpired(namespace, name string, tr *apispipeline.TaskRun) error {
	tc.Logger.Infof("Checking if TaskRun %s/%s is ready for cleanup", namespace, name)
	if tr.HasPipelineRunOwnerReference() {
		return nil
	}

	if expired, err := tc.processTrTTL(tr); err != nil {
		return err
	} else if !expired {
		return nil
	}

	// The TaskRun's TTL is assumed to have expired, but the TaskRun TTL might be stale.
	// Before deleting the TaskRun, do a final sanity check.
	// If TTL is modified before we do this check, we cannot be sure if the TTL truly expires.
	// The latest TaskRun may have a different UID, but it's fine because the checks will be run again.
	fresh, err := tc.PipelineClientSet.TektonV1alpha1().TaskRuns(namespace).Get(name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if fresh.HasPipelineRunOwnerReference() {
		return nil
	}
	// Use the latest TaskRun TTL to see if the TTL truly expires.
	if expired, err := tc.processTrTTL(fresh); err != nil {
		return err
	} else if !expired {
		return nil
	}
	// Cascade deletes the TaskRuns if TTL truly expires.
	policy := metav1.DeletePropagationForeground
	options := &metav1.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &fresh.UID},
	}
	tc.Logger.Infof("Cleaning up TaskRun %s/%s", namespace, name)
	return tc.PipelineClientSet.TektonV1alpha1().TaskRuns(fresh.Namespace).Delete(fresh.Name, options)
}

// processTTL checks whether a given TaskRun's TTL has expired, and add it to the queue after the TTL is expected to expire
// if the TTL will expire later.
func (tc *Reconciler) processTrTTL(tr *apispipeline.TaskRun) (expired bool, err error) {
	// We don't care about the TaskRuns that are going to be deleted, or the ones that don't need clean up.
	if tr.DeletionTimestamp != nil || !taskRunCleanup(tr) {
		return false, nil
	}

	now := tc.clock.Now()
	t, err := tc.trTimeLeft(tr, &now)
	if err != nil {
		return false, err
	}

	// TTL has expired
	if *t <= 0 {
		return true, nil
	}

	controller.NewImpl(tc, tc.Logger, ControllerName).EnqueueAfter(tr, *t)
	return false, nil
}

func getFinishAndExpireTime(tr *apispipeline.TaskRun) (*time.Time, *time.Time, error) {
	if !taskRunCleanup(tr) {
		return nil, nil, fmt.Errorf("TaskRun %s/%s should not be cleaned up", tr.Namespace, tr.Name)
	}
	finishAt, err := taskRunFinishTime(tr)
	if err != nil {
		return nil, nil, err
	}
	finishAtUTC := finishAt.Inner.UTC()
	expireAtUTC := finishAtUTC.Add(tr.Spec.ExpirationSecondsTTL.Duration)
	return &finishAtUTC, &expireAtUTC, nil
}

func (tc *Reconciler) trTimeLeft(tr *apispipeline.TaskRun, since *time.Time) (*time.Duration, error) {
	finishAt, expireAt, err := getFinishAndExpireTime(tr)
	if err != nil {
		return nil, err
	}
	if finishAt.UTC().After(since.UTC()) {
		tc.Logger.Warnf("Warning: Found taskRun %s/%s succeeded in the future. This is likely due to time skew in the cluster. taskrun cleanup will be deferred.", tr.Namespace, tr.Name)
	}

	remaining := expireAt.UTC().Sub(since.UTC())
	tc.Logger.Infof("Found taskRun %s/%s succeeded at %v, remaining TTL %v since %v, TTL will expire at %v\n", tr.Namespace, tr.Name, finishAt.UTC(), remaining, since.UTC(), expireAt.UTC())

	return &remaining, nil
}

// taskRunFinishTime takes an already succeeded taskRun and returns the time it finishes.
func taskRunFinishTime(tr *apispipeline.TaskRun) (apis.VolatileTime, error) {
	for _, con := range tr.Status.Conditions {
		if con.Type == apis.ConditionSucceeded && con.Status != v1.ConditionUnknown {
			finishAt := con.LastTransitionTime
			if finishAt.Inner.IsZero() {
				return apis.VolatileTime{}, fmt.Errorf("unable to find the time when the taskRun %s/%s succeeded", tr.Namespace, tr.Name)
			}
			return con.LastTransitionTime, nil
		}
	}

	// This should never happen if the taskRuns has succeeded
	return apis.VolatileTime{}, fmt.Errorf("unable to find the status of the succeeded taskRun %s/%s", tr.Namespace, tr.Name)
}

// taskRunCleanup checks whether a TaskRun has succeeded or failed and has a TTL set.
func taskRunCleanup(tr *apispipeline.TaskRun) bool {
	return tr.Spec.ExpirationSecondsTTL != nil && tr.IsDone() && !tr.HasPipelineRunOwnerReference()
}
