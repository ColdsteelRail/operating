/*
Copyright 2024 The KusionStack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package operationjob

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "kusionstack.io/operating/apis/apps/v1alpha1"
	"kusionstack.io/operating/pkg/controllers/collaset/podcontrol"
	ojutils "kusionstack.io/operating/pkg/controllers/operationjob/utils"
)

type TargetOperator interface {
	ListTargets() ([]*OpsCandidate, error)
	OperateTarget(*OpsCandidate) error
	FulfilPodOpsStatus(*OpsCandidate) error
	ReleaseTarget(*OpsCandidate) (*time.Duration, error)
}

type GenericOperator struct {
	ctx          context.Context
	logger       logr.Logger
	client       client.Client
	recorder     record.EventRecorder
	operationJob *appsv1alpha1.OperationJob
}

type OpsCandidate struct {
	pod          *corev1.Pod
	podName      string
	containers   []string
	podOpsStatus *appsv1alpha1.PodOpsStatus

	replaceTriggered bool
	replaceNewPod    *corev1.Pod
	collaSet         *appsv1alpha1.CollaSet
}

func (r *ReconcileOperationJob) newOperator(ctx context.Context, instance *appsv1alpha1.OperationJob, logger logr.Logger) TargetOperator {
	mixin := r.ReconcilerMixin
	genericOperator := &GenericOperator{client: mixin.Client, ctx: ctx, operationJob: instance, logger: logger, recorder: mixin.Recorder}

	switch instance.Spec.Action {
	case appsv1alpha1.ActionRecreate:
		recreateMethodAnno := instance.ObjectMeta.Annotations[appsv1alpha1.AnnotationOperationJobRecreateMethod]
		if recreateMethodAnno == "" || GetRecreateHandler(recreateMethodAnno) == nil {
			// use Kruise ContainerRecreateRequest to recreate container by default
			return &containerRestartOperator{GenericOperator: genericOperator, handler: GetRecreateHandler(string(appsv1alpha1.CRRKey))}
		}
		return &containerRestartOperator{GenericOperator: genericOperator, handler: GetRecreateHandler(recreateMethodAnno)}
	case appsv1alpha1.OpsActionReplace:
		return &podReplaceOperator{GenericOperator: genericOperator,
			podControl: podcontrol.NewRealPodControl(r.ReconcilerMixin.Client, r.ReconcilerMixin.Scheme)}
	default:
		panic(fmt.Errorf("unsupported operation type %s", instance.Spec.Action))
	}
}

func (r *ReconcileOperationJob) ensureActiveDeadlineOrTTL(ctx context.Context, instance *appsv1alpha1.OperationJob, logger logr.Logger) (bool, *time.Duration, error) {
	isFailed := instance.Status.Progress == appsv1alpha1.OperationProgressFailed
	isCompleted := instance.Status.Progress == appsv1alpha1.OperationProgressCompleted

	if instance.Spec.ActiveDeadlineSeconds != nil {
		if !isFailed && !isCompleted {
			leftTime := time.Duration(*instance.Spec.ActiveDeadlineSeconds)*time.Second - time.Since(instance.CreationTimestamp.Time)
			if leftTime > 0 {
				return false, &leftTime, nil
			} else {
				logger.Info("should end but still processing")
				r.Recorder.Eventf(instance, corev1.EventTypeNormal, "Timeout", "Try to fail operationJob for timeout...")
				ojutils.MarkOperationJobFailed(instance)
				return false, nil, nil
			}
		}
	}

	if instance.Spec.TTLSecondsAfterFinished != nil {
		if isFailed || isCompleted {
			leftTime := time.Duration(*instance.Spec.TTLSecondsAfterFinished)*time.Second - time.Since(instance.Status.EndTimestamp.Time)
			if leftTime > 0 {
				return false, &leftTime, nil
			} else {
				logger.Info("should be deleted but still alive")
				r.Recorder.Eventf(instance, corev1.EventTypeNormal, "TTL", "Try to delete operationJob for TTL...")
				err := r.Client.Delete(ctx, instance)
				return true, nil, err
			}
		}
	}

	return false, nil, nil
}

func (r *ReconcileOperationJob) ReleaseTargetsForDeletion(ctx context.Context, instance *appsv1alpha1.OperationJob, logger logr.Logger) (*time.Duration, error) {
	ojutils.MarkOperationJobFailed(instance)
	operator := r.newOperator(ctx, instance, logger)
	candidates, err := operator.ListTargets()
	if err != nil {
		return nil, err
	}

	var requeueAfter *time.Duration
	for _, candidate := range candidates {
		duration, err := operator.ReleaseTarget(candidate)
		if duration != nil {
			if requeueAfter == nil {
				requeueAfter = duration
			} else if *duration < *requeueAfter {
				requeueAfter = duration
			}
		}
		if err != nil {
			return requeueAfter, err
		}
	}
	return requeueAfter, nil

}

func decideCandidateByPartition(instance *appsv1alpha1.OperationJob, candidates []*OpsCandidate) []*OpsCandidate {
	if instance.Spec.Partition == nil {
		return candidates
	}
	ordered := activeCandidateToStart(candidates)
	sort.Sort(ordered)

	partition := int(*instance.Spec.Partition)
	if partition >= len(candidates) {
		return candidates
	}
	return candidates[:partition]
}

type activeCandidateToStart []*OpsCandidate

func (o activeCandidateToStart) Len() int {
	return len(o)
}

func (o activeCandidateToStart) Swap(i, j int) {
	o[i], o[j] = o[j], o[i]
}

func (o activeCandidateToStart) Less(i, j int) bool {
	l, r := o[i], o[j]
	lNotStarted := isCandidateOpsNotStarted(l)
	rNotStarted := isCandidateOpsNotStarted(r)
	if lNotStarted != rNotStarted {
		return rNotStarted
	}
	return true
}

func isCandidateOpsNotStarted(candidate *OpsCandidate) bool {
	if candidate.podOpsStatus == nil || candidate.podOpsStatus.Phase == "" {
		return true
	}
	return candidate.podOpsStatus.Phase == appsv1alpha1.PodPhaseNotStarted
}

func isCandidateOpsFinished(candidate *OpsCandidate) bool {
	if candidate.podOpsStatus == nil || candidate.podOpsStatus.Phase == "" {
		return false
	}
	return candidate.podOpsStatus.Phase == appsv1alpha1.PodPhaseCompleted ||
		candidate.podOpsStatus.Phase == appsv1alpha1.PodPhaseFailed
}
