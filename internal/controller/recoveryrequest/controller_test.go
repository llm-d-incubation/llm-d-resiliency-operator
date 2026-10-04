package recoveryrequest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/llm-d-incubation/llm-d-resiliency-operator/apis/inference/v1alpha1"
	"github.com/llm-d-incubation/llm-d-resiliency-operator/internal/controller/recoveryrequest"
)

const (
	completionAnnotation = "inference-resilience-operator.llm-d.io/completion-time"
	servingLabel         = "llm-d.ai/inference-serving"
)

func TestPendingLabelsPodsAndUpdatesCondition(t *testing.T) {
	for _, phase := range []string{"", "Pending"} {
		t.Run("phase="+phase, func(t *testing.T) {
			rr := recoveryRequest(phase)
			rr.Status.Conditions = []metav1.Condition{
				{Type: "Other", Status: metav1.ConditionTrue, Reason: "Preserved"},
				{
					Type: "EngineReadyForRecovery", Status: metav1.ConditionFalse,
					ObservedGeneration: 7, Reason: "Old", LastTransitionTime: metav1.NewTime(time.Unix(1, 0)),
				},
			}
			selected := pod("selected", "workload", "target-node", "true")
			alreadyLabeled := pod("already-labeled", "workload", "target-node", "false")
			otherNode := pod("other-node", "workload", "other-node", "true")
			otherNamespace := pod("other-namespace", "elsewhere", "target-node", "true")
			otherLabels := pod("other-labels", "workload", "target-node", "true")
			otherLabels.Labels["app"] = "unselected"
			updates := make(map[string]int)
			reconciler := testReconciler(t, rr, &interceptor.Funcs{
				Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object,
					opts ...client.UpdateOption,
				) error {
					updates[obj.GetName()]++
					return underlying.Update(ctx, obj, opts...)
				},
			}, node(), selected, alreadyLabeled, otherNode, otherNamespace, otherLabels)

			result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
			if err != nil || result != (ctrl.Result{}) {
				t.Fatalf("reconcile = %v, %v", result, err)
			}
			for _, expected := range []struct {
				pod     *corev1.Pod
				serving string
			}{
				{selected, "false"},
				{alreadyLabeled, "false"},
				{otherNode, "false"},
				{otherNamespace, "true"},
				{otherLabels, "true"},
			} {
				assertPodServing(t, reconciler.Client, expected.pod, expected.serving)
			}
			if updates[selected.Name] != 1 || updates[otherNode.Name] != 1 || updates[alreadyLabeled.Name] != 0 {
				t.Fatalf("pod updates = %v", updates)
			}

			getObject(t, reconciler.Client, rr)
			if rr.Status.Phase != phase || len(rr.Status.Conditions) != 2 {
				t.Fatalf("status = %+v", rr.Status)
			}
			condition := rr.Status.Conditions[1]
			if condition.Status != metav1.ConditionTrue || condition.Reason != "NodeFound" ||
				condition.Message != "Node target-node is present in cluster" || condition.ObservedGeneration != 7 ||
				!condition.LastTransitionTime.After(time.Unix(1, 0)) || rr.Status.Conditions[0].Reason != "Preserved" {
				t.Fatalf("conditions = %+v", rr.Status.Conditions)
			}
		})
	}
}

func TestPendingAddsConditionAndInitializesPodLabels(t *testing.T) {
	rr := recoveryRequest("Pending")
	selected := pod("unlabeled", "workload", "target-node", "")
	selected.Labels = nil
	reconciler := testReconciler(t, rr, &interceptor.Funcs{}, node(), selected)
	reconciler.PodLabelKey = ""
	if _, err := reconciler.Reconcile(context.Background(), requestFor(rr)); err != nil {
		t.Fatal(err)
	}
	assertPodServing(t, reconciler.Client, selected, "false")
	getObject(t, reconciler.Client, rr)
	if len(rr.Status.Conditions) != 1 || rr.Status.Conditions[0].Type != "EngineReadyForRecovery" ||
		rr.Status.Conditions[0].Status != metav1.ConditionTrue || rr.Status.Conditions[0].LastTransitionTime.IsZero() {
		t.Fatalf("status = %+v", rr.Status)
	}
}

func TestMissingNodeFailsRequestWithoutLabeling(t *testing.T) {
	rr := recoveryRequest("Pending")
	rr.Status.Conditions = []metav1.Condition{{Type: "Other", Status: metav1.ConditionTrue, Reason: "Preserved"}}
	selected := pod("selected", "workload", "target-node", "true")
	reconciler := testReconciler(t, rr, &interceptor.Funcs{}, selected)
	result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
	if err != nil || result != (ctrl.Result{}) {
		t.Fatalf("reconcile = %v, %v", result, err)
	}
	getObject(t, reconciler.Client, rr)
	if rr.Status.Phase != "Failed" || len(rr.Status.Conditions) != 2 {
		t.Fatalf("status = %+v", rr.Status)
	}
	condition := rr.Status.Conditions[1]
	if condition.Type != "NodePresent" || condition.Status != metav1.ConditionFalse ||
		condition.Reason != "NodeNotFound" || condition.Message != "Node target-node not found in cluster" ||
		condition.LastTransitionTime.IsZero() || rr.Status.Conditions[0].Reason != "Preserved" {
		t.Fatalf("conditions = %+v", rr.Status.Conditions)
	}
	assertPodServing(t, reconciler.Client, selected, "true")
}

func TestCompletedRestoresServingAndRecordsTime(t *testing.T) {
	rr := recoveryRequest("Completed")
	rr.Annotations = map[string]string{"other": "preserved"}
	selected := pod("selected", "workload", "target-node", "false")
	alreadyLabeled := pod("already-labeled", "workload", "target-node", "true")
	reconciler := testReconciler(t, rr, &interceptor.Funcs{}, selected, alreadyLabeled)
	before := time.Now().Add(-time.Second)
	result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
	if err != nil || result != (ctrl.Result{}) {
		t.Fatalf("first reconcile = %v, %v", result, err)
	}
	assertPodServing(t, reconciler.Client, selected, "true")
	assertPodServing(t, reconciler.Client, alreadyLabeled, "true")
	getObject(t, reconciler.Client, rr)
	completed, err := time.Parse(time.RFC3339, rr.Annotations[completionAnnotation])
	if err != nil || completed.Before(before) || completed.After(time.Now()) ||
		rr.Annotations["other"] != "preserved" || rr.Status.Phase != "Completed" {
		t.Fatalf("request = %+v, parsed time = %v, %v", rr, completed, err)
	}

	result, err = reconciler.Reconcile(context.Background(), requestFor(rr))
	if err != nil || result.RequeueAfter < 4*time.Minute || result.RequeueAfter > 5*time.Minute {
		t.Fatalf("second reconcile = %v, %v", result, err)
	}
	getObject(t, reconciler.Client, rr)
	if rr.Annotations[completionAnnotation] != completed.Format(time.RFC3339) {
		t.Fatalf("completion time changed: %v", rr.Annotations)
	}
}

func TestCompletedWithRecordedTime(t *testing.T) {
	for _, test := range []struct {
		name      string
		value     string
		wantError bool
		deleted   bool
	}{
		{"waiting", time.Now().Add(-time.Minute).Format(time.RFC3339), false, false},
		{"expired", time.Now().Add(-6 * time.Minute).Format(time.RFC3339), false, true},
		{"invalid", "not-a-time", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rr := recoveryRequest("Completed")
			rr.Annotations = map[string]string{completionAnnotation: test.value}
			selected := pod("selected", "workload", "target-node", "false")
			reconciler := testReconciler(t, rr, &interceptor.Funcs{}, selected)
			result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
			if (err != nil) != test.wantError {
				t.Fatalf("reconcile = %v, %v", result, err)
			}
			if test.name == "waiting" && (result.RequeueAfter < 3*time.Minute || result.RequeueAfter > 4*time.Minute) {
				t.Fatalf("requeue = %v", result.RequeueAfter)
			}
			if test.name != "waiting" && result != (ctrl.Result{}) {
				t.Fatalf("result = %v", result)
			}
			getErr := reconciler.Get(context.Background(), client.ObjectKeyFromObject(rr), &inferencev1alpha1.RecoveryRequest{})
			if test.deleted && !apierrors.IsNotFound(getErr) || !test.deleted && getErr != nil {
				t.Fatalf("request lookup = %v, deleted = %v", getErr, test.deleted)
			}
			assertPodServing(t, reconciler.Client, selected, "false")
		})
	}
}

func TestMissingRequestAndInactivePhaseDoNothing(t *testing.T) {
	for _, phase := range []string{"Failed", "InProgress"} {
		t.Run(phase, func(t *testing.T) {
			rr := recoveryRequest(phase)
			selected := pod("selected", "workload", "target-node", "true")
			reconciler := testReconciler(t, rr, &interceptor.Funcs{}, selected)
			result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
			if err != nil || result != (ctrl.Result{}) {
				t.Fatalf("reconcile = %v, %v", result, err)
			}
			assertPodServing(t, reconciler.Client, selected, "true")
			missing := requestFor(rr)
			missing.Name = "missing"
			result, err = reconciler.Reconcile(context.Background(), missing)
			if err != nil || result != (ctrl.Result{}) {
				t.Fatalf("missing reconcile = %v, %v", result, err)
			}
		})
	}
}

func TestPodSelectionErrorsDoNotAdvanceRequest(t *testing.T) {
	for _, phase := range []string{"Pending", "Completed"} {
		for _, selector := range []string{"app=engine", "app in ("} {
			t.Run(phase+"/"+selector, func(t *testing.T) {
				rr := recoveryRequest(phase)
				reconciler := testReconciler(t, rr, &interceptor.Funcs{}, node())
				reconciler.PodLabelKey = selector
				result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
				if err == nil || result != (ctrl.Result{}) {
					t.Fatalf("reconcile = %v, %v", result, err)
				}
				if selector == "app=engine" && !strings.Contains(err.Error(), "no pods found") {
					t.Fatalf("error = %v", err)
				}
				getObject(t, reconciler.Client, rr)
				if len(rr.Status.Conditions) != 0 || len(rr.Annotations) != 0 || rr.Status.Phase != phase {
					t.Fatalf("request advanced: %+v", rr)
				}
			})
		}
	}
}

func TestClientFailuresAreReturned(t *testing.T) {
	wantErr := errors.New("injected client failure")
	for _, test := range []struct {
		name      string
		phase     string
		operation string
		noNode    bool
		expired   bool
	}{
		{"request get", "Pending", "request-get", false, false},
		{"node get", "Pending", "node-get", false, false},
		{"pending list", "Pending", "list", false, false},
		{"completed list", "Completed", "list", false, false},
		{"pending pod update", "Pending", "pod-update", false, false},
		{"completed pod update", "Completed", "pod-update", false, false},
		{"pending status update", "Pending", "status-update", false, false},
		{"missing node status update", "Pending", "status-update", true, false},
		{"completion annotation update", "Completed", "request-update", false, false},
		{"request deletion", "Completed", "delete", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rr := recoveryRequest(test.phase)
			if test.expired {
				rr.Annotations = map[string]string{
					completionAnnotation: time.Now().Add(-6 * time.Minute).Format(time.RFC3339),
				}
			}
			selected := pod("selected", "workload", "target-node", "opposite")
			objects := []client.Object{selected}
			if !test.noNode {
				objects = append(objects, node())
			}
			reconciler := testReconciler(t, rr, failingClient(test.operation, wantErr), objects...)
			result, err := reconciler.Reconcile(context.Background(), requestFor(rr))
			if !errors.Is(err, wantErr) || result != (ctrl.Result{}) {
				t.Fatalf("reconcile = %v, %v; want %v", result, err, wantErr)
			}
		})
	}
}

func failingClient(operation string, failure error) *interceptor.Funcs {
	return &interceptor.Funcs{
		Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			_, isNode := obj.(*corev1.Node)
			if operation == "node-get" && isNode || operation == "request-get" && !isNode {
				return failure
			}
			return underlying.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if operation == "list" {
				return failure
			}
			return underlying.List(ctx, list, opts...)
		},
		Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			_, isPod := obj.(*corev1.Pod)
			if operation == "pod-update" && isPod || operation == "request-update" && !isPod {
				return failure
			}
			return underlying.Update(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, underlying client.Client, name string, obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			if operation == "status-update" {
				return failure
			}
			return underlying.SubResource(name).Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if operation == "delete" {
				return failure
			}
			return underlying.Delete(ctx, obj, opts...)
		},
	}
}

func recoveryRequest(phase string) *inferencev1alpha1.RecoveryRequest {
	return &inferencev1alpha1.RecoveryRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "recovery", Namespace: "control"},
		Spec:       inferencev1alpha1.RecoveryRequestSpec{NodeName: "target-node", RequestedAction: "reset"},
		Status:     inferencev1alpha1.RecoveryRequestStatus{Phase: phase},
	}
}

func node() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "target-node"}}
}

func pod(name, namespace, nodeName, serving string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: map[string]string{"app": "engine", servingLabel: serving},
		},
		Spec: corev1.PodSpec{NodeName: nodeName},
	}
}

func testReconciler(t *testing.T, rr *inferencev1alpha1.RecoveryRequest,
	funcs *interceptor.Funcs, objects ...client.Object,
) *recoveryrequest.Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects = append(objects, rr)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(rr).WithInterceptorFuncs(*funcs).Build()
	return &recoveryrequest.Reconciler{
		Client: kubeClient, Scheme: scheme, PodNamespace: "workload", PodLabelKey: "app=engine",
	}
}

func requestFor(rr *inferencev1alpha1.RecoveryRequest) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
}

func getObject(t *testing.T, kubeClient client.Client, obj client.Object) {
	t.Helper()
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
}

func assertPodServing(t *testing.T, kubeClient client.Client, expected *corev1.Pod, serving string) {
	t.Helper()
	actual := expected.DeepCopy()
	getObject(t, kubeClient, actual)
	if actual.Labels[servingLabel] != serving {
		t.Fatalf("pod %s serving = %q, want %q", actual.Name, actual.Labels[servingLabel], serving)
	}
}
