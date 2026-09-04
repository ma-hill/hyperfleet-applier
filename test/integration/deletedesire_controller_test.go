//go:build envtest

package integration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-hyperfleet/hyperfleet-applier/internal/controllers/deletedesire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
)

// deletePollInterval is short enough that these tests don't spend most of
// their budget waiting on DeleteReconciler.Start's own ticker.
const deletePollInterval = 50 * time.Millisecond

// deleteWidgetGVR is TestEnvtest_DeleteDesire_NewCRDResolvedAutomatically's
// own Widget API group - see installWidgetCRD.
var deleteWidgetGVR = schema.GroupVersionResource{Group: "delete.hyperfleet.example.com", Version: "v1", Resource: "widgets"}

// TestEnvtest_DeleteDesire_SimpleCases covers deletedesire's three
// unconditional-success shapes against a real apiserver: an existing pod, an
// existing cluster-scoped resource, and a resource that was already gone.
func TestEnvtest_DeleteDesire_SimpleCases(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T) desire.Identity
		verify func(t *testing.T, id desire.Identity)
	}{
		{
			name: "Pod",
			setup: func(t *testing.T) desire.Identity {
				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns-simple"}}
				if err := envK8sClient.Create(context.Background(), ns); err != nil {
					t.Fatalf("create namespace: %v", err)
				}
				t.Cleanup(func() { cleanupNamespace(t, ns.Name) })
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "test-ns-simple"},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
					},
				}
				if err := envK8sClient.Create(context.Background(), pod); err != nil {
					t.Fatalf("create pod: %v", err)
				}
				return identity(desire.TypeDelete, "", "pods", "test-ns-simple", "test-pod")
			},
			verify: func(t *testing.T, id desire.Identity) {
				var pod corev1.Pod
				err := envK8sClient.Get(context.Background(), client.ObjectKey{
					Namespace: id.Namespace, Name: id.Name,
				}, &pod)
				if !apierrors.IsNotFound(err) {
					t.Errorf("Get() error = %v, want NotFound: pod must be deleted", err)
				}
			},
		},
		{
			name: "ClusterRole",
			setup: func(t *testing.T) desire.Identity {
				const name = "test-clusterrole-envtest"
				cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
				if err := envK8sClient.Create(context.Background(), cr); err != nil {
					t.Fatalf("create ClusterRole: %v", err)
				}
				return clusterRoleIdentity(desire.TypeDelete, name)
			},
			verify: func(t *testing.T, id desire.Identity) {
				var cr rbacv1.ClusterRole
				err := envK8sClient.Get(context.Background(), client.ObjectKey{Name: id.Name}, &cr)
				if !apierrors.IsNotFound(err) {
					t.Errorf("Get() error = %v, want NotFound: ClusterRole must be deleted", err)
				}
			},
		},
		{
			name: "NonExistentPod",
			setup: func(t *testing.T) desire.Identity {
				return identity(desire.TypeDelete, "", "pods", defaultNamespace, "non-existent-pod")
			},
			verify: func(t *testing.T, id desire.Identity) {
				var pod corev1.Pod
				err := envK8sClient.Get(context.Background(), client.ObjectKey{
					Namespace: id.Namespace, Name: id.Name,
				}, &pod)
				if !apierrors.IsNotFound(err) {
					t.Errorf("Get() error = %v, want NotFound: the pod must never have existed", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.setup(t)

			store := memory.New()
			d := desire.DeleteDesire{Identity: id, Owner: testOwner}
			if _, err := store.CreateDeleteDesire(context.Background(), d); err != nil {
				t.Fatalf("CreateDeleteDesire: %v", err)
			}

			r := deletedesire.New(store, store, envDynamicClient, envRESTMapper, testManagementCluster, deletePollInterval)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() { _ = r.Start(ctx) }()

			updated := waitForDeleteReason(t, ctx, store, id, desire.ReasonDeleted)
			if len(updated.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(updated.Status.Conditions))
			}
			cond := updated.Status.Conditions[0]
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("condition = %+v, want Status=True", cond)
			}

			tc.verify(t, id)
		})
	}
}

// TestEnvtest_DeleteDesire_WaitsForFinalizers proves that when a resource
// has a blocking finalizer, deletedesire records WaitingForDeletion instead
// of Deleted - the DELETE call is accepted, but the resource is not yet gone.
func TestEnvtest_DeleteDesire_WaitsForFinalizers(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns-finalizers"}}
	if err := envK8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { cleanupNamespace(t, ns.Name) })

	const podFinalizer = "test.finalizer/block-deletion"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-pod-finalizer",
			Namespace:  "test-ns-finalizers",
			Finalizers: []string{podFinalizer},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest"}},
		},
	}
	if err := envK8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	// Registered after the namespace cleanup, so it runs first (t.Cleanup is
	// LIFO): the finalizer must be gone before the namespace delete is
	// attempted.
	t.Cleanup(func() { removeFinalizer(t, pod.Namespace, pod.Name, podFinalizer) })

	store := memory.New()
	r := deletedesire.New(store, store, envDynamicClient, envRESTMapper, testManagementCluster, deletePollInterval)

	id := identity(desire.TypeDelete, "", "pods", "test-ns-finalizers", "test-pod-finalizer")
	if _, err := store.CreateDeleteDesire(context.Background(), desire.DeleteDesire{Identity: id, Owner: testOwner}); err != nil {
		t.Fatalf("CreateDeleteDesire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.Start(ctx) }()

	updated := waitForDeleteReason(t, ctx, store, id, desire.ReasonWaitingForDeletion)
	if len(updated.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(updated.Status.Conditions))
	}
	cond := updated.Status.Conditions[0]
	if cond.Type != desire.TypeSuccessful {
		t.Errorf("condition type = %q, want %q", cond.Type, desire.TypeSuccessful)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
	}
	if cond.Reason != desire.ReasonWaitingForDeletion {
		t.Errorf("condition reason = %q, want %q", cond.Reason, desire.ReasonWaitingForDeletion)
	}
	if cond.Message == "" {
		t.Error("expected message to contain deletion timestamp and UID")
	}

	var stillExistingPod corev1.Pod
	err := envK8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "test-ns-finalizers", Name: "test-pod-finalizer",
	}, &stillExistingPod)
	if err != nil {
		t.Errorf("expected pod to still exist with deletion timestamp, but got error: %v", err)
	}
	if stillExistingPod.DeletionTimestamp == nil {
		t.Error("expected pod to have deletion timestamp set")
	}
}

// TestEnvtest_DeleteDesire_NewCRDResolvedAutomatically proves
// setupResourceClient's own IsNoMatchError -> Reset() -> retry recovers on
// its own when a CRD is installed after the shared RESTMapper's discovery
// cache was already populated - no external Reset() call needed, matching
// applydesire's and readdesire's identical policy.
func TestEnvtest_DeleteDesire_NewCRDResolvedAutomatically(t *testing.T) {
	const name = "widget-envtest-delete"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := memory.New()
	r := deletedesire.New(store, store, envDynamicClient, envRESTMapper, testManagementCluster, deletePollInterval)

	id := widgetIdentity(desire.TypeDelete, deleteWidgetGVR, defaultNamespace, name)
	if _, err := store.CreateDeleteDesire(ctx, desire.DeleteDesire{Identity: id, Owner: testOwner}); err != nil {
		t.Fatalf("CreateDeleteDesire: %v", err)
	}

	go func() { _ = r.Start(ctx) }()

	// 1. The Widget CRD doesn't exist yet: GVR resolution fails even after
	// setupResourceClient's own internal Reset()-and-retry.
	waitForDeleteReason(t, ctx, store, id, desire.ReasonPreCheckFailed)

	// 2. Install the CRD and create a real Widget instance.
	installWidgetCRD(t, deleteWidgetGVR, apiextensionsv1.NamespaceScoped)
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": deleteWidgetGVR.Group + "/" + deleteWidgetGVR.Version,
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": defaultNamespace},
	}}
	createTarget(t, ctx, deleteWidgetGVR, defaultNamespace, widget)

	// 3. No external Reset() - setupResourceClient's own internal retry
	// picks up the new CRD on a later pass.
	waitForDeleteReason(t, ctx, store, id, desire.ReasonDeleted)
}
