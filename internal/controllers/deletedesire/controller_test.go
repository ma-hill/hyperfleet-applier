package deletedesire

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
)

type notifyingSpecLister struct {
	called chan<- struct{}
}

func (n notifyingSpecLister) ListDeleteDesires(context.Context, string) ([]desire.DeleteDesire, error) {
	n.called <- struct{}{}
	return nil, nil
}

func (notifyingSpecLister) GetDeleteDesire(
	context.Context, desire.Identity,
) (desire.DeleteDesire, error) {
	return desire.DeleteDesire{}, desire.ErrNotFound
}

const (
	rbacGroup = "rbac.authorization.k8s.io"
)

// completedDeleteSpecStore makes the re-check GetDeleteDesire report doneID as
// already Deleted, simulating a concurrent update completing the deletion after
// the desire was listed but before this pass reaches it.
type completedDeleteSpecStore struct {
	desire.SpecStore
	doneID desire.Identity
}

func (s *completedDeleteSpecStore) GetDeleteDesire(
	ctx context.Context, id desire.Identity,
) (desire.DeleteDesire, error) {
	dd, err := s.SpecStore.GetDeleteDesire(ctx, id)
	if err != nil {
		return dd, err
	}
	if id == s.doneID {
		dd.Status = deleted(desire.Status{}) // IsDeleted(dd.Status) == true
	}
	return dd, nil
}

// Testing error mapping scenarios

// noMatchMapper always returns NoMatchError - simulates CRD uninstalled.
type noMatchMapper struct {
	meta.RESTMapper
}

func (m *noMatchMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, &meta.NoResourceMatchError{PartialResource: resource}
}

func (m *noMatchMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return nil, &meta.NoKindMatchError{GroupKind: gk}
}

func (m *noMatchMapper) Reset() {
	// No-op for test
}

// errorMapper returns a generic error (not NoMatchError) from KindFor.
type errorMapper struct {
	meta.RESTMapper
}

func (m *errorMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, fmt.Errorf("simulated mapper error")
}

func (m *errorMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return nil, fmt.Errorf("simulated mapper error")
}

func (m *errorMapper) Reset() {
	// No-op for test
}

// Fake test dynamic client
func newFakeDynamicClient(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register corev1: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register rbacv1: %v", err)
	}
	return dynamicfake.NewSimpleDynamicClient(scheme, objs...)
}

// Fake test mapper
func newTestMapper() meta.RESTMapper {
	dm := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "", Version: "v1"},
		{Group: rbacGroup, Version: "v1"},
	})
	dm.AddSpecific(
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmap"},
		meta.RESTScopeNamespace,
	)
	dm.AddSpecific(
		schema.GroupVersionKind{Group: rbacGroup, Version: "v1", Kind: "ClusterRole"},
		schema.GroupVersionResource{Group: rbacGroup, Version: "v1", Resource: "clusterroles"},
		schema.GroupVersionResource{Group: rbacGroup, Version: "v1", Resource: "clusterrole"},
		meta.RESTScopeRoot,
	)
	return dm
}

// TestDeleteReconcileAll tests the DeleteReconciler's reconcileAll method across
// success and failure scenarios: successful deletion of namespaced and cluster-scoped
// resources, idempotent behavior when resources are already deleted, pre-check validation
// failures (invalid namespace, mapper errors), and special handling when resource types
// no longer exist (CRD uninstalled).
func TestDeleteReconcileAll(t *testing.T) {
	testManagementCluster := "test-cluster"
	testStore := memory.New()
	testMapper := newTestMapper()
	testOwner := "test-owner"
	testNamespace := "default"
	configMapsType := "configmaps"
	t.Run("DeletesNamespacedResourceSuccessfully", func(t *testing.T) {

		testCtx := t.Context()
		testDynamicClient := newFakeDynamicClient(t,
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: "default"},
			},
		)

		dr := New(testStore, testStore, testDynamicClient, testMapper, testManagementCluster, time.Hour)

		testDesire := desire.DeleteDesire{
			Identity: desire.Identity{
				Type:              desire.TypeDelete,
				ManagementCluster: testManagementCluster,
				Group:             "",
				Resource:          configMapsType,
				Namespace:         testNamespace,
				Name:              "test-cm",
			},
			Owner:   testOwner,
			Version: 1,
		}
		testDesire, err := testStore.CreateDeleteDesire(testCtx, testDesire)
		if err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		if err = dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		updatedDesire, err := testStore.GetDeleteDesire(testCtx, testDesire.Identity)
		if err != nil {
			t.Fatalf("GetDeleteDesire() error = %v", err)
		}

		if len(updatedDesire.Status.Conditions) == 0 {
			t.Fatalf("Status.Conditions is empty, should be updated")
		}
		if updatedDesire.Status.Conditions[0].Type != desire.TypeSuccessful {
			t.Errorf("Condition 0: Type is not Successful, should be Successful")
		}
		if updatedDesire.Status.Conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("Condition 0: Status is not True, should be True")
		}
		if updatedDesire.Status.Conditions[0].Reason != desire.ReasonDeleted {
			t.Errorf("Condition 0: Reason is not Deleted, should be Deleted")
		}
	})

	t.Run("DeletesClusterScopedResourceSuccessfully", func(t *testing.T) {

		testCtx := t.Context()

		testDynamicClient := newFakeDynamicClient(t,
			&rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{Name: "test-clusterrole"},
			},
		)
		dr := New(testStore, testStore, testDynamicClient, testMapper, testManagementCluster, time.Hour)

		testDesire := desire.DeleteDesire{
			Identity: desire.Identity{
				Type:              desire.TypeDelete,
				ManagementCluster: testManagementCluster,
				Group:             rbacGroup,
				Resource:          "clusterroles",
				Namespace:         "",
				Name:              "test-clusterrole",
			},
			Owner:   testOwner,
			Version: 1,
		}
		testDesire, err := testStore.CreateDeleteDesire(testCtx, testDesire)
		if err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		if err = dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		updatedDesire, err := testStore.GetDeleteDesire(testCtx, testDesire.Identity)
		if err != nil {
			t.Fatalf("GetDeleteDesire() error = %v", err)
		}

		if len(updatedDesire.Status.Conditions) == 0 {
			t.Fatalf("Status.Conditions is empty, should be updated")
		}
		if updatedDesire.Status.Conditions[0].Type != desire.TypeSuccessful {
			t.Errorf("Condition 0: Type is not Successful, should be Successful")
		}
		if updatedDesire.Status.Conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("Condition 0: Status is not True, should be True")
		}
		if updatedDesire.Status.Conditions[0].Reason != desire.ReasonDeleted {
			t.Errorf("Condition 0: Reason is not Deleted, should be Deleted")
		}
	})

	t.Run("SkipsAllApiserverCallsWhenDesireAlreadyRecordedDeleted", func(t *testing.T) {
		testCtx := t.Context()

		// A live resource still exists on the cluster...
		testDynamicClient := newFakeDynamicClient(t,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm-done", Namespace: testNamespace}},
		)

		// ...but the re-check reports the desire already completed Deleted, so
		// the guard must short-circuit before touching the apiserver at all -
		// not even the GET. Fail on any action.
		var apiCalled bool
		testDynamicClient.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
			apiCalled = true
			return false, nil, nil // fall through to the default reactor
		})

		baseStore := memory.New()
		id := desire.Identity{
			Type:              desire.TypeDelete,
			ManagementCluster: testManagementCluster,
			Group:             "",
			Resource:          configMapsType,
			Namespace:         testNamespace,
			Name:              "cm-done",
		}
		if _, err := baseStore.CreateDeleteDesire(testCtx, desire.DeleteDesire{
			Identity: id, Owner: testOwner, Version: 1,
		}); err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		dr := New(
			&completedDeleteSpecStore{SpecStore: baseStore, doneID: id},
			baseStore, testDynamicClient, testMapper, testManagementCluster, time.Hour,
		)

		if err := dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		if apiCalled {
			t.Error("made an apiserver call for a desire already recorded Deleted; the re-check must skip it entirely")
		}
	})

	t.Run("SucceedsWithoutDeleteCallWhenResourceAlreadyGone", func(t *testing.T) {
		testCtx := t.Context()

		// Don't seed any resources - the target is already gone from the
		// cluster, so executeDelete's first GET returns NotFound.
		testDynamicClient := newFakeDynamicClient(t)

		// Fail loudly if a DELETE is ever issued: a resource confirmed absent
		// on the first GET must short-circuit to ReasonDeleted without one.
		var deleteIssued bool
		testDynamicClient.PrependReactor("delete", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
			deleteIssued = true
			return false, nil, nil // fall through to the default reactor
		})

		dr := New(testStore, testStore, testDynamicClient, testMapper, testManagementCluster, time.Hour)

		testDesire := desire.DeleteDesire{
			Identity: desire.Identity{
				Type:              desire.TypeDelete,
				ManagementCluster: testManagementCluster,
				Group:             "",
				Resource:          configMapsType,
				Namespace:         testNamespace,
				Name:              "nonexistent-cm",
			},
			Owner:   testOwner,
			Version: 1,
		}
		testDesire, err := testStore.CreateDeleteDesire(testCtx, testDesire)
		if err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		if err = dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		if deleteIssued {
			t.Error("DELETE was issued for an already-absent resource; first GET must short-circuit to Deleted")
		}

		updatedDesire, err := testStore.GetDeleteDesire(testCtx, testDesire.Identity)
		if err != nil {
			t.Fatalf("GetDeleteDesire() error = %v", err)
		}

		if len(updatedDesire.Status.Conditions) == 0 {
			t.Fatalf("Status.Conditions is empty, should be updated")
		}
		if updatedDesire.Status.Conditions[0].Type != desire.TypeSuccessful {
			t.Errorf("Condition 0: Type = %s, want Successful", updatedDesire.Status.Conditions[0].Type)
		}
		if updatedDesire.Status.Conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("Condition 0: Status = %s, want True", updatedDesire.Status.Conditions[0].Status)
		}
		if updatedDesire.Status.Conditions[0].Reason != desire.ReasonDeleted {
			t.Errorf("Condition 0: Reason = %s, want Deleted", updatedDesire.Status.Conditions[0].Reason)
		}
	})

	t.Run("PreCheckFailsWhenNamespacedResourceHasEmptyNamespace", func(t *testing.T) {
		testCtx := t.Context()

		testDynamicClient := newFakeDynamicClient(t)
		dr := New(testStore, testStore, testDynamicClient, testMapper, testManagementCluster, time.Hour)

		testDesire := desire.DeleteDesire{
			Identity: desire.Identity{
				Type:              desire.TypeDelete,
				ManagementCluster: testManagementCluster,
				Group:             "",
				Resource:          configMapsType,
				Namespace:         "", // Invalid: ConfigMap is namespaced!
				Name:              "test-cm-invalid",
			},
			Owner:   testOwner,
			Version: 1,
		}
		testDesire, err := testStore.CreateDeleteDesire(testCtx, testDesire)
		if err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		if err = dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		updatedDesire, err := testStore.GetDeleteDesire(testCtx, testDesire.Identity)
		if err != nil {
			t.Fatalf("GetDeleteDesire() error = %v", err)
		}

		if len(updatedDesire.Status.Conditions) == 0 {
			t.Fatalf("Status.Conditions is empty, should be updated")
		}
		if updatedDesire.Status.Conditions[0].Type != desire.TypeSuccessful {
			t.Errorf("Condition 0: Type = %s, want Successful", updatedDesire.Status.Conditions[0].Type)
		}
		if updatedDesire.Status.Conditions[0].Status != metav1.ConditionFalse {
			t.Errorf("Condition 0: Status = %s, want False", updatedDesire.Status.Conditions[0].Status)
		}
		if updatedDesire.Status.Conditions[0].Reason != desire.ReasonPreCheckFailed {
			t.Errorf("Condition 0: Reason = %s, want PreCheckFailed", updatedDesire.Status.Conditions[0].Reason)
		}
	})

	t.Run("PreCheckFailsWhenResourceTypeDoesNotExist", func(t *testing.T) {
		testCtx := t.Context()

		testDynamicClient := newFakeDynamicClient(t)
		// Use a mapper that always returns NoMatchError (CRD uninstalled)
		noMatchMapper := &noMatchMapper{}
		dr := New(testStore, testStore, testDynamicClient, noMatchMapper, testManagementCluster, time.Hour)

		testDesire := desire.DeleteDesire{
			Identity: desire.Identity{
				Type:              desire.TypeDelete,
				ManagementCluster: testManagementCluster,
				Group:             "example.com",
				Resource:          "customresources",
				Namespace:         testNamespace,
				Name:              "my-custom-resource",
			},
			Owner:   testOwner,
			Version: 1,
		}
		testDesire, err := testStore.CreateDeleteDesire(testCtx, testDesire)
		if err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		if err = dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		updatedDesire, err := testStore.GetDeleteDesire(testCtx, testDesire.Identity)
		if err != nil {
			t.Fatalf("GetDeleteDesire() error = %v", err)
		}

		if len(updatedDesire.Status.Conditions) == 0 {
			t.Fatalf("Status.Conditions is empty, should be updated")
		}
		if updatedDesire.Status.Conditions[0].Type != desire.TypeSuccessful {
			t.Errorf("Condition 0: Type = %s, want Successful", updatedDesire.Status.Conditions[0].Type)
		}
		if updatedDesire.Status.Conditions[0].Status != metav1.ConditionFalse {
			t.Errorf("Condition 0: Status = %s, want False", updatedDesire.Status.Conditions[0].Status)
		}
		if updatedDesire.Status.Conditions[0].Reason != desire.ReasonPreCheckFailed {
			t.Errorf("Condition 0: Reason = %s, want PreCheckFailed", updatedDesire.Status.Conditions[0].Reason)
		}
	})

	t.Run("PreCheckFailsWhenMapperReturnsGenericError", func(t *testing.T) {
		testCtx := t.Context()

		testDynamicClient := newFakeDynamicClient(t)
		// Use a mapper that returns a generic error (not NoMatchError)
		errMapper := &errorMapper{}
		dr := New(testStore, testStore, testDynamicClient, errMapper, testManagementCluster, time.Hour)

		testDesire := desire.DeleteDesire{
			Identity: desire.Identity{
				Type:              desire.TypeDelete,
				ManagementCluster: testManagementCluster,
				Group:             "",
				Resource:          configMapsType,
				Namespace:         testNamespace,
				Name:              "test-cm-negative",
			},
			Owner:   testOwner,
			Version: 1,
		}
		testDesire, err := testStore.CreateDeleteDesire(testCtx, testDesire)
		if err != nil {
			t.Fatalf("CreateDeleteDesire() error = %v", err)
		}

		if err = dr.reconcileAll(testCtx); err != nil {
			t.Fatalf("reconcileAll() error = %v, should be nil", err)
		}

		updatedDesire, err := testStore.GetDeleteDesire(testCtx, testDesire.Identity)
		if err != nil {
			t.Fatalf("GetDeleteDesire() error = %v", err)
		}

		if len(updatedDesire.Status.Conditions) == 0 {
			t.Fatalf("Status.Conditions is empty, should be updated")
		}
		if updatedDesire.Status.Conditions[0].Type != desire.TypeSuccessful {
			t.Errorf("Condition 0: Type = %s, want Successful", updatedDesire.Status.Conditions[0].Type)
		}
		if updatedDesire.Status.Conditions[0].Status != metav1.ConditionFalse {
			t.Errorf("Condition 0: Status = %s, want False", updatedDesire.Status.Conditions[0].Status)
		}
		if updatedDesire.Status.Conditions[0].Reason != desire.ReasonPreCheckFailed {
			t.Errorf("Condition 0: Reason = %s, want PreCheckFailed", updatedDesire.Status.Conditions[0].Reason)
		}
	})
}

func TestStart_ReconcilesImmediatelyAndStopsCleanly(t *testing.T) {
	called := make(chan struct{}, 1)
	r := New(notifyingSpecLister{called: called}, nil, nil, nil, "test-cluster", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx)
	}()

	select {
	case <-called:
		// The first pass ran without waiting for the 60-second ticker.
	case <-time.After(time.Second):
		t.Fatal("Start did not begin a reconciliation pass immediately")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v, want nil after caller cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not stop after context cancellation")
	}
}
