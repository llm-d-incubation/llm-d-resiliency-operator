package recoveryrequest

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/llm-d-incubation/llm-d-resiliency-operator/apis/inference/v1alpha1"
)

const completionTimeAnnotation = "inference-resilience-operator.llm-d.io/completion-time"

// Reconciler reconciles a RecoveryRequest object.
type Reconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	PodNamespace string
	PodLabelKey  string
}

// +kubebuilder:rbac:groups=inference.llm-d.io,resources=recoveryrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llm-d.io,resources=recoveryrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile reads the object and makes changes to it if needed.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.Infof("Reconciling RecoveryRequest: %s", req.NamespacedName)

	var rr inferencev1alpha1.RecoveryRequest
	if err := r.Get(ctx, req.NamespacedName, &rr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	klog.Infof("Detected RecoveryRequest: Name=%s, Node=%s, Action=%s, Phase=%s",
		rr.Name, rr.Spec.NodeName, rr.Spec.RequestedAction, rr.Status.Phase)

	switch rr.Status.Phase {
	case "Pending", "":
		return ctrl.Result{}, r.reconcilePending(ctx, &rr)
	case "Completed":
		return r.reconcileCompleted(ctx, &rr)
	default:
		return ctrl.Result{}, nil
	}
}

func (r *Reconciler) reconcilePending(ctx context.Context, rr *inferencev1alpha1.RecoveryRequest) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: rr.Spec.NodeName}, &node); err != nil {
		if errors.IsNotFound(err) {
			return r.markNodeMissing(ctx, rr)
		}
		klog.Errorf("Failed to get node %s: %v", rr.Spec.NodeName, err)
		return err
	}

	klog.Infof("Node %s found for RecoveryRequest %s", rr.Spec.NodeName, rr.Name)
	if err := r.labelPods(ctx, "false"); err != nil {
		return err
	}

	markEngineReady(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		klog.Errorf("Failed to update status with condition: %v", err)
		return err
	}
	return nil
}

func (r *Reconciler) markNodeMissing(ctx context.Context, rr *inferencev1alpha1.RecoveryRequest) error {
	klog.Warningf("Node %s not found for RecoveryRequest %s", rr.Spec.NodeName, rr.Name)
	rr.Status.Phase = "Failed"
	rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{
		Type:               "NodePresent",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeNotFound",
		Message:            "Node " + rr.Spec.NodeName + " not found in cluster",
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		klog.Errorf("Failed to update status to Failed: %v", err)
		return err
	}
	return nil
}

func markEngineReady(rr *inferencev1alpha1.RecoveryRequest) {
	for i, cond := range rr.Status.Conditions {
		if cond.Type != "EngineReadyForRecovery" {
			continue
		}
		rr.Status.Conditions[i].Status = metav1.ConditionTrue
		rr.Status.Conditions[i].LastTransitionTime = metav1.Now()
		rr.Status.Conditions[i].Reason = "NodeFound"
		rr.Status.Conditions[i].Message = "Node " + rr.Spec.NodeName + " is present in cluster"
		return
	}
	rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{
		Type:               "EngineReadyForRecovery",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeFound",
		Message:            "Node " + rr.Spec.NodeName + " is present in cluster",
	})
}

func (r *Reconciler) labelPods(ctx context.Context, serving string) error {
	selector, err := labels.Parse(r.PodLabelKey)
	if err != nil {
		klog.Errorf("Failed to parse label key %s: %v", r.PodLabelKey, err)
		return err
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(r.PodNamespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		klog.Errorf("Failed to list pods: %v", err)
		return err
	}
	if len(podList.Items) == 0 {
		klog.Warningf("No pods found in namespace %s with selector %s", r.PodNamespace, r.PodLabelKey)
		return fmt.Errorf("no pods found in namespace %s with selector %s", r.PodNamespace, r.PodLabelKey)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		klog.Infof("Found pod %s to label %s", pod.Name, serving)
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		if pod.Labels["llm-d.ai/inference-serving"] == serving {
			klog.Infof("Pod %s already labeled %s, skipping update", pod.Name, serving)
			continue
		}
		pod.Labels["llm-d.ai/inference-serving"] = serving
		if err := r.Update(ctx, pod); err != nil {
			klog.Errorf("Failed to update pod %s: %v", pod.Name, err)
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileCompleted(ctx context.Context,
	rr *inferencev1alpha1.RecoveryRequest,
) (ctrl.Result, error) {
	completionTimeStr, ok := rr.Annotations[completionTimeAnnotation]
	if !ok {
		return ctrl.Result{}, r.markCompletionTime(ctx, rr)
	}

	completionTime, err := time.Parse(time.RFC3339, completionTimeStr)
	if err != nil {
		klog.Errorf("Failed to parse completion time %s: %v", completionTimeStr, err)
		return ctrl.Result{}, err
	}
	if time.Since(completionTime) >= 5*time.Minute {
		klog.Infof("Deleting RecoveryRequest %s after 5 minutes", rr.Name)
		if err := r.Delete(ctx, rr); err != nil {
			klog.Errorf("Failed to delete RecoveryRequest: %v", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	remainingTime := 5*time.Minute - time.Since(completionTime)
	klog.Infof("Requeuing RecoveryRequest %s for deletion in %v", rr.Name, remainingTime)
	return ctrl.Result{RequeueAfter: remainingTime}, nil
}

func (r *Reconciler) markCompletionTime(ctx context.Context, rr *inferencev1alpha1.RecoveryRequest) error {
	if err := r.labelPods(ctx, "true"); err != nil {
		return err
	}

	klog.Infof("Marking RecoveryRequest %s with completion time", rr.Name)
	if rr.Annotations == nil {
		rr.Annotations = make(map[string]string)
	}
	rr.Annotations[completionTimeAnnotation] = time.Now().Format(time.RFC3339)
	if err := r.Update(ctx, rr); err != nil {
		klog.Errorf("Failed to update annotations: %v", err)
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.RecoveryRequest{}).
		Complete(r)
}
