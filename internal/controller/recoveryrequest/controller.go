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

	inferencev1alpha1 "github.com/llm-d/llm-d-resiliency-operator/apis/inference/v1alpha1"
)

// Reconciler reconciles a RecoveryRequest object
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	PodNamespace string
	PodLabelKey  string
}

// +kubebuilder:rbac:groups=inference.llm-d.io,resources=recoveryrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.llm-d.io,resources=recoveryrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile reads that of the object and makes changes to the object if needed
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.Infof("Reconciling RecoveryRequest: %s", req.NamespacedName)

	var rr inferencev1alpha1.RecoveryRequest
	if err := r.Get(ctx, req.NamespacedName, &rr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	klog.Infof("Detected RecoveryRequest: Name=%s, Node=%s, Action=%s, Phase=%s",
		rr.Name, rr.Spec.NodeName, rr.Spec.RequestedAction, rr.Status.Phase)

	if rr.Status.Phase == "Pending" || rr.Status.Phase == "" {
		var node corev1.Node
		err := r.Get(ctx, client.ObjectKey{Name: rr.Spec.NodeName}, &node)
		if err != nil {
			if errors.IsNotFound(err) {
				klog.Warningf("Node %s not found for RecoveryRequest %s", rr.Spec.NodeName, rr.Name)
				rr.Status.Phase = "Failed"
				// Add a condition explaining why it failed
				rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{
					Type:               "NodePresent",
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
					Reason:             "NodeNotFound",
					Message:            "Node " + rr.Spec.NodeName + " not found in cluster",
				})
				if err := r.Status().Update(ctx, &rr); err != nil {
					klog.Errorf("Failed to update status to Failed: %v", err)
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
			klog.Errorf("Failed to get node %s: %v", rr.Spec.NodeName, err)
			return ctrl.Result{}, err
		}

		klog.Infof("Node %s found for RecoveryRequest %s", rr.Spec.NodeName, rr.Name)
		
		// Find pod matching configured namespace with configured label key on that node
		var podList corev1.PodList
		selector, err := labels.Parse(r.PodLabelKey)
		if err != nil {
			klog.Errorf("Failed to parse label key %s: %v", r.PodLabelKey, err)
			return ctrl.Result{}, err
		}
		
		err = r.List(ctx, &podList, client.InNamespace(r.PodNamespace), client.MatchingLabelsSelector{Selector: selector})
		if err != nil {
			klog.Errorf("Failed to list pods: %v", err)
			return ctrl.Result{}, err
		}

		if len(podList.Items) == 0 {
			klog.Warningf("No pods found in namespace %s with selector %s", r.PodNamespace, r.PodLabelKey)
			return ctrl.Result{}, fmt.Errorf("no pods found in namespace %s with selector %s", r.PodNamespace, r.PodLabelKey)
		}

		for i := range podList.Items {
			pod := &podList.Items[i]
			klog.Infof("Found pod %s to label false", pod.Name)
			if pod.Labels == nil {
				pod.Labels = make(map[string]string)
			}
			if pod.Labels["llm-d.ai/inference-serving"] == "false" {
				klog.Infof("Pod %s already labeled false, skipping update", pod.Name)
				continue
			}
			pod.Labels["llm-d.ai/inference-serving"] = "false"
			if err := r.Update(ctx, pod); err != nil {
				klog.Errorf("Failed to update pod %s: %v", pod.Name, err)
				return ctrl.Result{}, err
			}

		}


		// Node found, add EngineReadyForRecovery: true
		found := false
		for i, cond := range rr.Status.Conditions {
			if cond.Type == "EngineReadyForRecovery" {
				rr.Status.Conditions[i].Status = metav1.ConditionTrue
				rr.Status.Conditions[i].LastTransitionTime = metav1.Now()
				rr.Status.Conditions[i].Reason = "NodeFound"
				rr.Status.Conditions[i].Message = "Node " + rr.Spec.NodeName + " is present in cluster"
				found = true
				break
			}
		}
		if !found {
			rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{
				Type:               "EngineReadyForRecovery",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "NodeFound",
				Message:            "Node " + rr.Spec.NodeName + " is present in cluster",
			})
		}

		if err := r.Status().Update(ctx, &rr); err != nil {
			klog.Errorf("Failed to update status with condition: %v", err)
			return ctrl.Result{}, err
		}
	}

	if rr.Status.Phase == "Completed" {
		// Handle delayed deletion
		const CompletionTimeAnnotation = "inference-resilience-operator.llm-d.io/completion-time"
		completionTimeStr, ok := rr.Annotations[CompletionTimeAnnotation]
		var completionTime time.Time
		if !ok {
			// Find pod matching configured namespace with configured label key on that node
			var podList corev1.PodList
			selector, err := labels.Parse(r.PodLabelKey)
			if err != nil {
				klog.Errorf("Failed to parse label key %s: %v", r.PodLabelKey, err)
				return ctrl.Result{}, err
			}
			
			err = r.List(ctx, &podList, client.InNamespace(r.PodNamespace), client.MatchingLabelsSelector{Selector: selector})
			if err != nil {
				klog.Errorf("Failed to list pods: %v", err)
				return ctrl.Result{}, err
			}

			if len(podList.Items) == 0 {
				klog.Warningf("No pods found in namespace %s with selector %s", r.PodNamespace, r.PodLabelKey)
				return ctrl.Result{}, fmt.Errorf("no pods found in namespace %s with selector %s", r.PodNamespace, r.PodLabelKey)
			}

			for i := range podList.Items {
				pod := &podList.Items[i]
				klog.Infof("Found pod %s to label true", pod.Name)
				if pod.Labels == nil {
					pod.Labels = make(map[string]string)
				}
				if pod.Labels["llm-d.ai/inference-serving"] == "true" {
					klog.Infof("Pod %s already labeled true, skipping update", pod.Name)
					continue
				}
				pod.Labels["llm-d.ai/inference-serving"] = "true"
				if err := r.Update(ctx, pod); err != nil {
					klog.Errorf("Failed to update pod %s: %v", pod.Name, err)
					return ctrl.Result{}, err
				}

			}


			klog.Infof("Marking RecoveryRequest %s with completion time", rr.Name)
			completionTime = time.Now()
			if rr.Annotations == nil {
				rr.Annotations = make(map[string]string)
			}
			rr.Annotations[CompletionTimeAnnotation] = completionTime.Format(time.RFC3339)
			if err := r.Update(ctx, &rr); err != nil {
				klog.Errorf("Failed to update annotations: %v", err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		} else {
			var err error
			completionTime, err = time.Parse(time.RFC3339, completionTimeStr)
			if err != nil {
				klog.Errorf("Failed to parse completion time %s: %v", completionTimeStr, err)
				return ctrl.Result{}, err
			}
		}

		if time.Since(completionTime) >= 5*time.Minute {
			klog.Infof("Deleting RecoveryRequest %s after 5 minutes", rr.Name)
			if err := r.Delete(ctx, &rr); err != nil {
				klog.Errorf("Failed to delete RecoveryRequest: %v", err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		remainingTime := 5*time.Minute - time.Since(completionTime)
		klog.Infof("Requeuing RecoveryRequest %s for deletion in %v", rr.Name, remainingTime)
		return ctrl.Result{RequeueAfter: remainingTime}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.RecoveryRequest{}).
		Complete(r)
}
