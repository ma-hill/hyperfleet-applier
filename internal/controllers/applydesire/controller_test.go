package applydesire

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
)

const (
	testManagementCluster     = "test-cluster"
	rbacGroup                 = "rbac.authorization.k8s.io"
	defaultNamespace          = "default"
	fieldAPIVersion           = "apiVersion"
	fieldMetadata             = "metadata"
	fieldName                 = "name"
	fieldNamespace            = "namespace"
	fieldKind                 = "kind"
	kindConfigMap             = "ConfigMap"
	resourceConfigMapSingular = "configmap"
)

// ---- fixtures & helpers -----------------------------------------------

var (
	configMapGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: kindConfigMap}
	configMapGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	clusterRoleGVK = schema.GroupVersionKind{Group: rbacGroup, Version: "v1", Kind: "ClusterRole"}
	clusterRoleGVR = schema.GroupVersionResource{Group: rbacGroup, Version: "v1", Resource: "clusterroles"}
)

// newTestMapper returns a RESTMapper that knows about a namespaced ConfigMap
// and a cluster-scoped ClusterRole, matching real Kubernetes RESTMapping
// behavior for those two kinds.
func newTestMapper() meta.RESTMapper {
	dm := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "", Version: "v1"},
		{Group: rbacGroup, Version: "v1"},
	})
	dm.AddSpecific(
		configMapGVK,
		configMapGVR,
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: resourceConfigMapSingular},
		meta.RESTScopeNamespace,
	)
	dm.AddSpecific(
		clusterRoleGVK,
		clusterRoleGVR,
		schema.GroupVersionResource{Group: rbacGroup, Version: "v1", Resource: "clusterrole"},
		meta.RESTScopeRoot,
	)
	return dm
}

// unrecoverableMappingMapper always returns NoMatchError from RESTMapping to
// exercise the permanent mapping-failure path in applyToCluster.
type unrecoverableMappingMapper struct {
	*meta.DefaultRESTMapper
}

func newUnrecoverableMappingMapper() meta.RESTMapper {
	return &unrecoverableMappingMapper{
		DefaultRESTMapper: meta.NewDefaultRESTMapper(nil),
	}
}

func (m *unrecoverableMappingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return nil, &meta.NoKindMatchError{GroupKind: gk}
}

// newFakeScheme registers concrete ConfigMap and ClusterRole types for test seeding.
// The fake tracker's Apply path uses StrategicMergePatch with the stored object's Go
// type as the merge schema; bare Unstructured lacks patch struct tags, so SSA apply
// against fake configmaps/clusterroles fails without concrete seeded types.
func newFakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register corev1: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register rbacv1: %v", err)
	}
	return scheme
}

// newFakeDynamicClient builds a fake dynamic client seeded with objs,
// preserving their concrete Go types (see newFakeScheme).
func newFakeDynamicClient(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(newFakeScheme(t), nil, objs...)
}

func newConfigMapObject(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func newConfigMapContent(t *testing.T, name, namespace string, data map[string]string) json.RawMessage {
	t.Helper()
	dataAny := map[string]interface{}{}
	for k, v := range data {
		dataAny[k] = v
	}
	raw, err := json.Marshal(map[string]interface{}{
		fieldAPIVersion: "v1",
		fieldKind:       kindConfigMap,
		fieldMetadata: map[string]interface{}{
			fieldName:      name,
			fieldNamespace: namespace,
		},
		"data": dataAny,
	})
	if err != nil {
		t.Fatalf("marshal configmap content: %v", err)
	}
	return raw
}

// newConfigMapContentNoNamespace builds a ConfigMap manifest whose metadata
// omits "namespace" entirely, to exercise applyToCluster applying in
// d.Identity.Namespace when the manifest does not name one.
func newConfigMapContentNoNamespace(t *testing.T, name string, data map[string]string) json.RawMessage {
	t.Helper()
	dataAny := map[string]interface{}{}
	for k, v := range data {
		dataAny[k] = v
	}
	raw, err := json.Marshal(map[string]interface{}{
		fieldAPIVersion: "v1",
		fieldKind:       kindConfigMap,
		fieldMetadata: map[string]interface{}{
			fieldName: name,
		},
		"data": dataAny,
	})
	if err != nil {
		t.Fatalf("marshal configmap content: %v", err)
	}
	return raw
}

func newClusterRoleObject(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func newClusterRoleContent(t *testing.T, name string) json.RawMessage {
	t.Helper()
	return newClusterRoleContentWithNamespace(t, name, "")
}

// newClusterRoleContentWithNamespace builds a ClusterRole manifest, optionally
// including metadata.namespace to exercise cluster-scoped pre-check behavior.
func newClusterRoleContentWithNamespace(t *testing.T, name, namespace string) json.RawMessage {
	t.Helper()
	metadata := map[string]interface{}{
		fieldName: name,
	}
	if namespace != "" {
		metadata[fieldNamespace] = namespace
	}
	raw, err := json.Marshal(map[string]interface{}{
		fieldAPIVersion: rbacGroup + "/v1",
		fieldKind:       "ClusterRole",
		fieldMetadata:   metadata,
		"rules":         []interface{}{},
	})
	if err != nil {
		t.Fatalf("marshal clusterrole content: %v", err)
	}
	return raw
}

func applyIdentity(group, resource, namespace, name string) desire.Identity {
	return desire.Identity{
		ManagementCluster: testManagementCluster,
		Type:              desire.TypeApply,
		Group:             group,
		Resource:          resource,
		Namespace:         namespace,
		Name:              name,
	}
}

func seedApplyDesire(
	t *testing.T, store desire.SpecStore, id desire.Identity, owner string, content json.RawMessage,
) desire.ApplyDesire {
	t.Helper()
	d, err := store.CreateApplyDesire(context.Background(), desire.ApplyDesire{
		Identity: id,
		Owner:    owner,
		Spec:     desire.ApplySpec{KubeContent: content},
	})
	if err != nil {
		t.Fatalf("CreateApplyDesire(%+v): %v", id, err)
	}
	return d
}

func findPatchAction(actions []k8stesting.Action, gvr schema.GroupVersionResource) *k8stesting.PatchActionImpl {
	var found *k8stesting.PatchActionImpl
	for _, a := range actions {
		p, ok := a.(k8stesting.PatchActionImpl)
		if !ok {
			continue
		}
		if p.GetPatchType() != types.ApplyPatchType {
			continue
		}
		if p.GetResource() != gvr {
			continue
		}
		pCopy := p
		found = &pCopy
	}
	return found
}

func countApplyPatchActions(actions []k8stesting.Action, gvr schema.GroupVersionResource) int {
	n := 0
	for _, a := range actions {
		p, ok := a.(k8stesting.PatchActionImpl)
		if !ok {
			continue
		}
		if p.GetPatchType() != types.ApplyPatchType {
			continue
		}
		if p.GetResource() != gvr {
			continue
		}
		n++
	}
	return n
}

func countActions(actions []k8stesting.Action, verb string, gvr schema.GroupVersionResource) int {
	n := 0
	for _, a := range actions {
		if a.GetVerb() != verb {
			continue
		}
		if a.GetResource() != gvr {
			continue
		}
		n++
	}
	return n
}

// ---- decorators used to observe / inject store behavior ----------------

type erroringSpecStore struct {
	desire.SpecStore
	listErr error
}

func (e *erroringSpecStore) ListApplyDesires(
	ctx context.Context, managementCluster string,
) ([]desire.ApplyDesire, error) {
	return nil, e.listErr
}

// staleApplySpecStore makes the re-check GetApplyDesire report goneID as gone
// (e.g. superseded by a DeleteDesire) after it was listed for this pass.
type staleApplySpecStore struct {
	desire.SpecStore
	goneID desire.Identity
}

func (s *staleApplySpecStore) GetApplyDesire(
	ctx context.Context, id desire.Identity,
) (desire.ApplyDesire, error) {
	if id == s.goneID {
		return desire.ApplyDesire{}, desire.ErrNotFound
	}
	return s.SpecStore.GetApplyDesire(ctx, id)
}

type notifyingSpecLister struct {
	called chan<- struct{}
}

func (n notifyingSpecLister) ListApplyDesires(context.Context, string) ([]desire.ApplyDesire, error) {
	n.called <- struct{}{}
	return nil, nil
}

func (n notifyingSpecLister) GetApplyDesire(context.Context, desire.Identity) (desire.ApplyDesire, error) {
	return desire.ApplyDesire{}, desire.ErrNotFound
}

type conflictingStatusStore struct {
	desire.StatusStore
	remainingConflicts int
}

func (c *conflictingStatusStore) UpdateApplyDesireStatus(
	ctx context.Context, id desire.Identity, status desire.Status, version int64,
) (desire.ApplyDesire, error) {
	if c.remainingConflicts > 0 {
		c.remainingConflicts--
		return desire.ApplyDesire{}, desire.ErrVersionConflict
	}
	return c.StatusStore.UpdateApplyDesireStatus(ctx, id, status, version)
}

// erroringStatusStore fails UpdateApplyDesireStatus for one identity with a
// non-version-conflict error, exercising reconcileOne's status-write error arm.
type erroringStatusStore struct {
	desire.StatusStore
	err    error
	failID desire.Identity
}

func (e *erroringStatusStore) UpdateApplyDesireStatus(
	ctx context.Context, id desire.Identity, status desire.Status, version int64,
) (desire.ApplyDesire, error) {
	if id == e.failID {
		return desire.ApplyDesire{}, e.err
	}
	return e.StatusStore.UpdateApplyDesireStatus(ctx, id, status, version)
}

// specBumpOnStatusConflictStore simulates a version race where the spec store
// moves forward (new manifest, bumped Version) while a reconcile pass still
// holds the list-time snapshot: apply runs against the listed manifest, then
// the status CAS fails.
type specBumpOnStatusConflictStore struct {
	desire.StatusStore
	spec         desire.SpecStore
	owner        string
	newSpec      desire.ApplySpec
	conflictOnce bool
}

func (s *specBumpOnStatusConflictStore) UpdateApplyDesireStatus(
	ctx context.Context, id desire.Identity, status desire.Status, version int64,
) (desire.ApplyDesire, error) {
	if s.conflictOnce {
		return s.StatusStore.UpdateApplyDesireStatus(ctx, id, status, version)
	}
	s.conflictOnce = true

	cur, err := s.spec.GetApplyDesire(ctx, id)
	if err != nil {
		return desire.ApplyDesire{}, err
	}
	if _, err := s.spec.UpdateApplyDesireSpec(ctx, id, s.newSpec, s.owner, cur.Version); err != nil {
		return desire.ApplyDesire{}, err
	}
	return desire.ApplyDesire{}, desire.ErrVersionConflict
}

type countingStatusStore struct {
	desire.StatusStore
	updateCalls int
}

func (c *countingStatusStore) UpdateApplyDesireStatus(
	ctx context.Context, id desire.Identity, status desire.Status, version int64,
) (desire.ApplyDesire, error) {
	c.updateCalls++
	return c.StatusStore.UpdateApplyDesireStatus(ctx, id, status, version)
}

// ---- tests ---------------------------------------------------------------

func TestStart_ReconcilesImmediatelyAndStopsCleanly(t *testing.T) {
	called := make(chan struct{}, 1)
	r := New(notifyingSpecLister{called: called}, nil, nil, nil, testManagementCluster, time.Hour)
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

func TestReconcileAll_AppliesNamespacedResourceSuccessfully(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-1", defaultNamespace))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-1")
	content := newConfigMapContent(t, "cm-1", defaultNamespace, map[string]string{"key": "value"})
	seedApplyDesire(t, store, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Status != metav1.ConditionTrue || c.Reason != desire.ReasonApplied {
		t.Errorf("condition = %+v, want Status=True Reason=%q", c, desire.ReasonApplied)
	}

	patch := findPatchAction(dyn.Actions(), configMapGVR)
	if patch == nil {
		t.Fatalf("no SSA apply patch action recorded against %v; actions=%v", configMapGVR, dyn.Actions())
	}
	if patch.GetNamespace() != defaultNamespace {
		t.Errorf("patch namespace = %q, want %q", patch.GetNamespace(), defaultNamespace)
	}
	if patch.PatchOptions.FieldManager != "hyperfleet-applier" {
		t.Errorf("PatchOptions.FieldManager = %q, want %q", patch.PatchOptions.FieldManager, "hyperfleet-applier")
	}
	if patch.PatchOptions.Force == nil || !*patch.PatchOptions.Force {
		t.Errorf("PatchOptions.Force = %v, want a pointer to true", patch.PatchOptions.Force)
	}
	if got := countActions(dyn.Actions(), "get", configMapGVR); got != 0 {
		t.Errorf("get actions against %v = %d, want 0 (Applied must not imply a post-apply read)", configMapGVR, got)
	}
}

func TestReconcileAll_AppliesClusterScopedResourceSuccessfully(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newClusterRoleObject("cr-1"))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity(rbacGroup, "clusterroles", "", "cr-1")
	seedApplyDesire(t, store, id, "owner-1", newClusterRoleContent(t, "cr-1"))

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != desire.ReasonApplied {
		t.Fatalf("condition = %+v, want Status=True Reason=%q", c, desire.ReasonApplied)
	}

	patch := findPatchAction(dyn.Actions(), clusterRoleGVR)
	if patch == nil {
		t.Fatalf("no SSA apply patch action recorded against %v; actions=%v", clusterRoleGVR, dyn.Actions())
	}
	if patch.GetNamespace() != "" {
		t.Errorf("patch namespace = %q, want empty for a cluster-scoped resource", patch.GetNamespace())
	}
}

// TestReconcileAll_ManifestMissingKindFailsToUnmarshal covers the
// json.Unmarshal-error branch of applyToCluster: a manifest missing "kind"
// fails *unstructured.Unstructured's own UnmarshalJSON (which requires Kind),
// before the apiVersion/kind/name precheck ever runs. See
// TestReconcileAll_IncompleteManifestGetsPreCheckFailedStatus for the
// precheck branch itself (valid-but-incomplete manifests that unmarshal
// cleanly).
func TestReconcileAll_ManifestMissingKindFailsToUnmarshal(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t)
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	// Valid JSON, but missing "kind" -- store.Validate() only requires
	// non-empty valid JSON, so this is a reachable "malformed manifest"
	// shape. Syntactically invalid JSON cannot be seeded here because
	// store validation rejects it at Create time.
	badContent, marshalErr := json.Marshal(map[string]interface{}{
		fieldAPIVersion: "v1",
		fieldMetadata: map[string]interface{}{
			fieldName:      "cm-bad",
			fieldNamespace: defaultNamespace,
		},
	})
	if marshalErr != nil {
		t.Fatalf("marshal bad content: %v", marshalErr)
	}

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-bad")
	seedApplyDesire(t, store, id, "owner-1", badContent)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil (a malformed desire must not abort the tick)", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Status = %v, want %v", c.Status, metav1.ConditionFalse)
	}
	if c.Reason != desire.ReasonPreCheckFailed {
		t.Errorf("Reason = %q, want %q", c.Reason, desire.ReasonPreCheckFailed)
	}
	const wantSubstring = "missing kind"
	if !strings.Contains(c.Message, wantSubstring) {
		t.Errorf("Message = %q, want it to contain %q", c.Message, wantSubstring)
	}

	if patch := findPatchAction(dyn.Actions(), configMapGVR); patch != nil {
		t.Errorf("malformed manifest should never reach the dynamic client, but got patch action: %+v", patch)
	}
}

// TestReconcileAll_IncompleteManifestGetsPreCheckFailedStatus covers the
// apiVersion/kind/name precheck in applyToCluster directly: manifests that
// unmarshal into *unstructured.Unstructured without error (unlike a
// missing-kind manifest, which fails unmarshal itself -- see
// TestReconcileAll_ManifestMissingKindFailsToUnmarshal) but are still missing
// a field the precheck requires.
func TestReconcileAll_IncompleteManifestGetsPreCheckFailedStatus(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t)
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	content, marshalErr := json.Marshal(map[string]interface{}{
		fieldAPIVersion: "v1",
		fieldKind:       kindConfigMap,
		fieldMetadata: map[string]interface{}{
			fieldNamespace: defaultNamespace,
		},
	})
	if marshalErr != nil {
		t.Fatalf("marshal content: %v", marshalErr)
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(content, obj); err != nil {
		t.Fatalf("fixture must unmarshal cleanly to exercise the precheck branch, got: %v", err)
	}

	const idName = "cm-no-name"
	id := applyIdentity("", "configmaps", defaultNamespace, idName)
	seedApplyDesire(t, store, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil (an incomplete desire must not abort the tick)", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Status = %v, want %v", c.Status, metav1.ConditionFalse)
	}
	if c.Reason != desire.ReasonPreCheckFailed {
		t.Errorf("Reason = %q, want %q", c.Reason, desire.ReasonPreCheckFailed)
	}
	if c.Message == "" {
		t.Errorf("Message is empty, want a useful diagnostic")
	}

	if patch := findPatchAction(dyn.Actions(), configMapGVR); patch != nil {
		t.Errorf("incomplete manifest should never reach the dynamic client, but got patch action: %+v", patch)
	}
}

// TestReconcileAll_MismatchedIdentityGetsPreCheckFailed proves that a desire
// whose KubeContent targets a different object than Identity is refused before
// any kube-apiserver call: the store does not cross-check those fields, so
// this is a reachable apply-time shape.
func TestReconcileAll_MismatchedIdentityGetsPreCheckFailed(t *testing.T) {
	cases := []struct {
		name    string
		id      desire.Identity
		content func(t *testing.T) json.RawMessage
		patchOn schema.GroupVersionResource
	}{
		{
			name: "name",
			id:   applyIdentity("", "configmaps", defaultNamespace, "cm-id"),
			content: func(t *testing.T) json.RawMessage {
				return newConfigMapContent(t, "cm-manifest", defaultNamespace, map[string]string{"k": "v"})
			},
			patchOn: configMapGVR,
		},
		{
			name: "namespace",
			id:   applyIdentity("", "configmaps", defaultNamespace, "cm-ns"),
			content: func(t *testing.T) json.RawMessage {
				return newConfigMapContent(t, "cm-ns", "other-ns", map[string]string{"k": "v"})
			},
			patchOn: configMapGVR,
		},
		{
			name: "empty identity namespace",
			id:   applyIdentity("", "configmaps", "", "cm-empty-ns"),
			content: func(t *testing.T) json.RawMessage {
				return newConfigMapContentNoNamespace(t, "cm-empty-ns", map[string]string{"k": "v"})
			},
			patchOn: configMapGVR,
		},
		{
			name: "gvr",
			id:   applyIdentity("", "secrets", defaultNamespace, "cm-gvr"),
			content: func(t *testing.T) json.RawMessage {
				return newConfigMapContent(t, "cm-gvr", defaultNamespace, map[string]string{"k": "v"})
			},
			patchOn: configMapGVR,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dyn := newFakeDynamicClient(t)
			store := memory.New()
			r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

			seedApplyDesire(t, store, tc.id, "owner-1", tc.content(t))

			if err := r.reconcileAll(ctx); err != nil {
				t.Fatalf("reconcileAll() error = %v, want nil (a mismatched desire must not abort the tick)", err)
			}

			got, err := store.GetApplyDesire(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetApplyDesire: %v", err)
			}
			c := findCondition(got.Status, desire.TypeSuccessful)
			if c == nil {
				t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
			}
			if c.Status != metav1.ConditionFalse {
				t.Errorf("Status = %v, want %v", c.Status, metav1.ConditionFalse)
			}
			if c.Reason != desire.ReasonPreCheckFailed {
				t.Errorf("Reason = %q, want %q", c.Reason, desire.ReasonPreCheckFailed)
			}
			if !strings.Contains(c.Message, "does not match identity") &&
				!strings.Contains(c.Message, "identity namespace must not be empty") {
				t.Errorf("Message = %q, want it to mention an identity mismatch", c.Message)
			}

			if patch := findPatchAction(dyn.Actions(), tc.patchOn); patch != nil {
				t.Errorf("mismatched identity should never reach the dynamic client, but got patch action: %+v", patch)
			}
		})
	}
}

// TestReconcileAll_ApplyErrorGetsKubeAPIErrorStatus proves the KubeAPIError
// arm of the condition contract is wired end to end: a real apiserver rejection
// (simulated via a fake-dynamic-client reactor) must result in
// Successful=False/KubeAPIError.
func TestReconcileAll_ApplyErrorGetsKubeAPIErrorStatus(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-apierr", defaultNamespace))
	applyErr := errors.New("apiserver unavailable")
	dyn.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, applyErr
	})
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-apierr")
	content := newConfigMapContent(t, "cm-apierr", defaultNamespace, map[string]string{"k": "v"})
	seedApplyDesire(t, store, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil (an apply failure is recorded on status, not returned)", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("Status = %v, want %v", c.Status, metav1.ConditionFalse)
	}
	if c.Reason != desire.ReasonKubeAPIError {
		t.Errorf("Reason = %q, want %q", c.Reason, desire.ReasonKubeAPIError)
	}
	if !strings.Contains(c.Message, applyErr.Error()) {
		t.Errorf("Message = %q, want it to contain the underlying error text %q", c.Message, applyErr.Error())
	}
}

// TestReconcileAll_CanceledContextBeforeApplyAborts proves that a context
// canceled before the pass reaches a desire aborts reconcileAll with the
// context error and records nothing: cancellation is caller control flow, not a
// resource failure.
func TestReconcileAll_CanceledContextBeforeApplyAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-cancel-before", defaultNamespace))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-cancel-before")
	seedApplyDesire(t, store, id, "owner-1",
		newConfigMapContent(t, "cm-cancel-before", defaultNamespace, map[string]string{"k": "v"}),
	)

	err := r.reconcileAll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcileAll() error = %v, want it to wrap context.Canceled", err)
	}

	got, getErr := store.GetApplyDesire(context.Background(), id)
	if getErr != nil {
		t.Fatalf("GetApplyDesire: %v", getErr)
	}
	if len(got.Status.Conditions) != 0 {
		t.Errorf("status = %+v, want unchanged (empty): cancellation must not record a status", got.Status)
	}
	if patch := findPatchAction(dyn.Actions(), configMapGVR); patch != nil {
		t.Errorf("cancellation before apply must not reach SSA, got patch: %+v", patch)
	}
}

// TestReconcileAll_ContextCanceledDuringApplyIsNotRecordedAsFailure proves that
// when the caller's context ends mid-apply, the applier aborts with the context
// error instead of overwriting the existing healthy status with KubeAPIError.
func TestReconcileAll_ContextCanceledDuringApplyIsNotRecordedAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-cancel-during", defaultNamespace))
	dyn.PrependReactor("patch", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return true, nil, context.Canceled
	})

	store := memory.New()
	id := applyIdentity("", "configmaps", defaultNamespace, "cm-cancel-during")
	seeded := seedApplyDesire(t, store, id, "owner-1",
		newConfigMapContent(t, "cm-cancel-during", defaultNamespace, map[string]string{"k": "v"}),
	)

	healthy := desire.Status{Conditions: []metav1.Condition{{
		Type:   desire.TypeSuccessful,
		Status: metav1.ConditionTrue,
		Reason: desire.ReasonApplied,
	}}}
	if _, err := store.UpdateApplyDesireStatus(context.Background(), id, healthy, seeded.Version); err != nil {
		t.Fatalf("seed healthy status: %v", err)
	}

	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)
	err := r.reconcileAll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcileAll() error = %v, want it to wrap context.Canceled", err)
	}

	got, getErr := store.GetApplyDesire(context.Background(), id)
	if getErr != nil {
		t.Fatalf("GetApplyDesire: %v", getErr)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != desire.ReasonApplied {
		t.Errorf(
			"status condition = %+v, want the pre-existing Successful=True/%q preserved (not overwritten with KubeAPIError)",
			c, desire.ReasonApplied,
		)
	}
}

func TestReconcileAll_MalformedDesireDoesNotAbortOthers(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-good", defaultNamespace))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	goodID := applyIdentity("", "configmaps", defaultNamespace, "cm-good")
	goodContent := newConfigMapContent(t, "cm-good", defaultNamespace, map[string]string{"k": "v"})
	seedApplyDesire(t, store, goodID, "owner-1", goodContent)

	badContent, marshalErr := json.Marshal(map[string]interface{}{
		fieldAPIVersion: "v1",
		fieldMetadata:   map[string]interface{}{fieldName: "cm-bad2", fieldNamespace: defaultNamespace},
	})
	if marshalErr != nil {
		t.Fatalf("marshal bad content: %v", marshalErr)
	}
	badID := applyIdentity("", "configmaps", defaultNamespace, "cm-bad2")
	seedApplyDesire(t, store, badID, "owner-1", badContent)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	good, err := store.GetApplyDesire(ctx, goodID)
	if err != nil {
		t.Fatalf("GetApplyDesire(good): %v", err)
	}
	goodCond := findCondition(good.Status, desire.TypeSuccessful)
	if goodCond == nil || goodCond.Status != metav1.ConditionTrue || goodCond.Reason != desire.ReasonApplied {
		t.Errorf(
			"good desire condition = %+v, want Status=True Reason=%q (a sibling failure must not abort this one)",
			goodCond, desire.ReasonApplied,
		)
	}

	bad, err := store.GetApplyDesire(ctx, badID)
	if err != nil {
		t.Fatalf("GetApplyDesire(bad): %v", err)
	}
	badCond := findCondition(bad.Status, desire.TypeSuccessful)
	if badCond == nil || badCond.Status != metav1.ConditionFalse || badCond.Reason != desire.ReasonPreCheckFailed {
		t.Errorf("bad desire condition = %+v, want Status=False Reason=%q", badCond, desire.ReasonPreCheckFailed)
	}
}

func TestReconcileAll_ToleratesStatusVersionConflict(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-cas", defaultNamespace))
	base := memory.New()
	status := &conflictingStatusStore{StatusStore: base, remainingConflicts: 1}
	r := New(base, status, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-cas")
	content := newConfigMapContent(t, "cm-cas", defaultNamespace, map[string]string{"k": "v"})
	seeded := seedApplyDesire(t, base, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf(
			"reconcileAll() error = %v, want nil: a status CAS conflict on one pass must be tolerated, not surfaced as a crash",
			err,
		)
	}

	// The conflicting write never actually committed, so the underlying
	// record must be untouched, exactly as if this pass never ran a status
	// write -- the next real pass is expected to retry it.
	got, err := base.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	if got.Version != seeded.Version {
		t.Errorf("Version = %d, want unchanged %d after a tolerated CAS conflict", got.Version, seeded.Version)
	}
	if len(got.Status.Conditions) != 0 {
		t.Errorf("Status = %+v, want unchanged (empty) after a tolerated CAS conflict", got.Status)
	}

	if patch := findPatchAction(dyn.Actions(), configMapGVR); patch == nil {
		t.Fatal("expected SSA apply to run before the tolerated status CAS conflict, but no patch action was recorded")
	} else if patch.GetNamespace() != defaultNamespace {
		t.Errorf("patch namespace = %q, want %q", patch.GetNamespace(), defaultNamespace)
	}

	obj, getErr := dyn.Resource(configMapGVR).Namespace(defaultNamespace).Get(ctx, "cm-cas", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("Get after reconcile with tolerated CAS conflict: %v", getErr)
	}
	val, found, nestedErr := unstructured.NestedString(obj.Object, "data", "k")
	if nestedErr != nil || !found {
		t.Fatalf("expected applied ConfigMap data.k, got found=%v err=%v obj=%+v", found, nestedErr, obj.Object)
	}
	if val != "v" {
		t.Errorf("data.k = %q, want %q: apply must complete before a failed status CAS", val, "v")
	}
}

// TestReconcileAll_StatusWriteErrorDoesNotAbortOthers proves that a
// reconcileOne failure from a non-version-conflict status write is joined
// into the error reconcileAll returns, without aborting sibling desires.
func TestReconcileAll_StatusWriteErrorDoesNotAbortOthers(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t,
		newConfigMapObject("cm-good-status", defaultNamespace),
		newConfigMapObject("cm-status-err", defaultNamespace),
	)
	base := memory.New()
	statusErr := errors.New("status store unavailable")

	goodID := applyIdentity("", "configmaps", defaultNamespace, "cm-good-status")
	badID := applyIdentity("", "configmaps", defaultNamespace, "cm-status-err")

	status := &erroringStatusStore{StatusStore: base, failID: badID, err: statusErr}
	r := New(base, status, dyn, newTestMapper(), testManagementCluster, time.Hour)

	seedApplyDesire(t, base, goodID, "owner-1",
		newConfigMapContent(t, "cm-good-status", defaultNamespace, map[string]string{"k": "good"}),
	)
	seedApplyDesire(t, base, badID, "owner-1",
		newConfigMapContent(t, "cm-status-err", defaultNamespace, map[string]string{"k": "bad"}),
	)

	err := r.reconcileAll(ctx)
	if err == nil {
		t.Fatal("reconcileAll() error = nil, want a status write failure to be reported to the caller")
	}
	if !errors.Is(err, statusErr) {
		t.Errorf("reconcileAll() error = %v, want it to wrap the underlying status store error %v", err, statusErr)
	}

	good, err := base.GetApplyDesire(ctx, goodID)
	if err != nil {
		t.Fatalf("GetApplyDesire(good): %v", err)
	}
	goodCond := findCondition(good.Status, desire.TypeSuccessful)
	if goodCond == nil || goodCond.Status != metav1.ConditionTrue || goodCond.Reason != desire.ReasonApplied {
		t.Errorf(
			"good desire condition = %+v, want Status=True Reason=%q (sibling must not be blocked)",
			goodCond, desire.ReasonApplied,
		)
	}

	bad, err := base.GetApplyDesire(ctx, badID)
	if err != nil {
		t.Fatalf("GetApplyDesire(bad): %v", err)
	}
	if len(bad.Status.Conditions) != 0 {
		t.Errorf("bad desire status = %+v, want unchanged (empty): status write failed after apply", bad.Status)
	}
	if got := countApplyPatchActions(dyn.Actions(), configMapGVR); got != 2 {
		t.Errorf("SSA apply patch count = %d, want 2 (both desires must reach apply before status write)", got)
	}
	obj, getErr := dyn.Resource(configMapGVR).Namespace(defaultNamespace).Get(ctx, "cm-status-err", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("Get bad desire object after reconcile: %v", getErr)
	}
	val, found, nestedErr := unstructured.NestedString(obj.Object, "data", "k")
	if nestedErr != nil || !found {
		t.Fatalf("expected applied ConfigMap data.k on bad desire, got found=%v err=%v", found, nestedErr)
	}
	if val != "bad" {
		t.Errorf("data.k on bad desire = %q, want %q: apply must succeed even when status write fails", val, "bad")
	}
}

// TestReconcileAll_ClusterScopedStrayNamespaceReachesApply proves
// checkApplyTarget does not reject metadata.namespace on cluster-scoped
// manifests; SSA still runs and the kube-apiserver accepts or rejects it.
func TestReconcileAll_ClusterScopedStrayNamespaceReachesApply(t *testing.T) {
	ctx := context.Background()
	const name = "cr-stray-ns"
	dyn := newFakeDynamicClient(t, newClusterRoleObject(name))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity(rbacGroup, "clusterroles", "", name)
	content := newClusterRoleContentWithNamespace(t, name, "stray-namespace")
	seedApplyDesire(t, store, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Reason == desire.ReasonPreCheckFailed {
		t.Fatalf(
			"Reason = %q, want not %q: applier must not reject stray namespace on cluster-scoped kinds",
			c.Reason, desire.ReasonPreCheckFailed,
		)
	}
	switch c.Reason {
	case desire.ReasonApplied, desire.ReasonKubeAPIError:
		// Fake client may reject the stray namespace at apply time; envtest covers
		// real apiserver acceptance. Either outcome proves precheck did not block SSA.
	default:
		t.Fatalf(
			"condition = %+v, want Reason=%q or %q after forwarding to SSA",
			c, desire.ReasonApplied, desire.ReasonKubeAPIError,
		)
	}

	patch := findPatchAction(dyn.Actions(), clusterRoleGVR)
	if patch == nil {
		t.Fatalf("expected SSA apply to reach the dynamic client, actions=%v", dyn.Actions())
	}
	if patch.GetNamespace() != "" {
		t.Errorf("patch namespace = %q, want empty for a cluster-scoped resource", patch.GetNamespace())
	}
}

// TestReconcileAll_ClusterScopedNonEmptyIdentityNamespaceGetsPreCheckFailed
// proves checkApplyTarget rejects a non-empty Identity.Namespace on a
// cluster-scoped kind. Namespace is part of the desire's target identity, so
// two desires that differ only in namespace would be distinct records both
// applying the same physical object and fighting under Force=true; the
// applier must never reach SSA in that case.
func TestReconcileAll_ClusterScopedNonEmptyIdentityNamespaceGetsPreCheckFailed(t *testing.T) {
	ctx := context.Background()
	const name = "cr-bad-ns"
	dyn := newFakeDynamicClient(t, newClusterRoleObject(name))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity(rbacGroup, "clusterroles", "ns-a", name)
	seedApplyDesire(t, store, id, "owner-1", newClusterRoleContent(t, name))

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Status != metav1.ConditionFalse || c.Reason != desire.ReasonPreCheckFailed {
		t.Errorf("condition = %+v, want Successful=False/%q", c, desire.ReasonPreCheckFailed)
	}
	if !strings.Contains(c.Message, "must be empty for cluster-scoped resource") {
		t.Errorf("Message = %q, want it to mention the cluster-scoped namespace rule", c.Message)
	}
	if patch := findPatchAction(dyn.Actions(), clusterRoleGVR); patch != nil {
		t.Errorf("cluster-scoped desire with a namespace must not reach SSA, but got patch: %+v", patch)
	}
}

// TestReconcileAll_StaleApplyWindowAfterStatusCASConflict covers the race where
// SSA succeeds against the list-time manifest but UpdateApplyDesireStatus loses
// a version conflict after the spec store has moved on: the cluster briefly
// holds the listed manifest until the next pass applies the updated spec.
func TestReconcileAll_StaleApplyWindowAfterStatusCASConflict(t *testing.T) {
	ctx := context.Background()
	const name = "cm-stale-window"
	dyn := newFakeDynamicClient(t, newConfigMapObject(name, defaultNamespace))
	base := memory.New()

	listed := newConfigMapContent(t, name, defaultNamespace, map[string]string{"k": "listed"})
	updated := newConfigMapContent(t, name, defaultNamespace, map[string]string{"k": "updated"})

	status := &specBumpOnStatusConflictStore{
		StatusStore: base,
		spec:        base,
		owner:       "owner-1",
		newSpec:     desire.ApplySpec{KubeContent: updated},
	}
	r := New(base, status, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, name)
	seeded := seedApplyDesire(t, base, id, "owner-1", listed)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() [pass 1] error = %v, want nil", err)
	}

	obj, getErr := dyn.Resource(configMapGVR).Namespace(defaultNamespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("Get after pass 1: %v", getErr)
	}
	val, found, nestedErr := unstructured.NestedString(obj.Object, "data", "k")
	if nestedErr != nil || !found {
		t.Fatalf("expected ConfigMap data.k after pass 1, got found=%v err=%v", found, nestedErr)
	}
	if val != "listed" {
		t.Errorf("data.k after pass 1 = %q, want %q (cluster must reflect the list-time manifest)", val, "listed")
	}

	afterConflict, err := base.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire after pass 1: %v", err)
	}
	if afterConflict.Version <= seeded.Version {
		t.Errorf("Version = %d, want > %d after spec bump during status CAS conflict", afterConflict.Version, seeded.Version)
	}
	if string(afterConflict.Spec.KubeContent) != string(updated) {
		t.Errorf("spec after pass 1 = %q, want updated manifest %q", afterConflict.Spec.KubeContent, updated)
	}
	if len(afterConflict.Status.Conditions) != 0 {
		t.Errorf("Status = %+v, want unchanged (empty) after tolerated CAS conflict", afterConflict.Status.Conditions)
	}

	if rcErr := r.reconcileAll(ctx); rcErr != nil {
		t.Fatalf("reconcileAll() [pass 2] error = %v, want nil", rcErr)
	}

	obj2, getErr := dyn.Resource(configMapGVR).Namespace(defaultNamespace).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("Get after pass 2: %v", getErr)
	}
	val2, found2, nestedErr2 := unstructured.NestedString(obj2.Object, "data", "k")
	if nestedErr2 != nil || !found2 {
		t.Fatalf("expected ConfigMap data.k after pass 2, got found=%v err=%v", found2, nestedErr2)
	}
	if val2 != "updated" {
		t.Errorf("data.k after pass 2 = %q, want %q (next pass must apply the updated spec)", val2, "updated")
	}

	afterSecond, err := base.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire after pass 2: %v", err)
	}
	c := findCondition(afterSecond.Status, desire.TypeSuccessful)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != desire.ReasonApplied {
		t.Errorf("condition after pass 2 = %+v, want Successful=True Reason=%q", c, desire.ReasonApplied)
	}
}

func TestReconcileAll_UnchangedDesireSuppressesStatusWrite(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-noop", defaultNamespace))
	base := memory.New()
	counting := &countingStatusStore{StatusStore: base}
	r := New(base, counting, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-noop")
	content := newConfigMapContent(t, "cm-noop", defaultNamespace, map[string]string{"k": "v"})
	seedApplyDesire(t, base, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() [pass 1] error = %v, want nil", err)
	}
	callsAfterFirstPass := counting.updateCalls
	if callsAfterFirstPass == 0 {
		t.Fatalf("expected the first reconcile of a new desire to write status at least once")
	}
	afterFirst, err := base.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire after pass 1: %v", err)
	}

	if rcErr := r.reconcileAll(ctx); rcErr != nil {
		t.Fatalf("reconcileAll() [pass 2] error = %v, want nil", rcErr)
	}

	if counting.updateCalls != callsAfterFirstPass {
		t.Errorf(
			"UpdateApplyDesireStatus called %d times after pass 2, want still %d: "+
				"reconciling an unchanged desire must suppress the redundant status write",
			counting.updateCalls, callsAfterFirstPass,
		)
	}
	afterSecond, err := base.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire after pass 2: %v", err)
	}
	if afterSecond.Version != afterFirst.Version {
		t.Errorf(
			"Version changed from %d to %d across an unchanged reconcile pass, want no-op",
			afterFirst.Version, afterSecond.Version,
		)
	}
}

func TestReconcileAll_ReturnsErrorOnListFailure(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	listErr := errors.New("store unavailable")
	spec := &erroringSpecStore{SpecStore: base, listErr: listErr}
	dyn := newFakeDynamicClient(t)

	r := New(spec, base, dyn, newTestMapper(), testManagementCluster, time.Hour)

	if err := r.reconcileAll(ctx); err == nil {
		t.Fatalf("reconcileAll() error = nil, want non-nil when SpecStore.ListApplyDesires fails")
	}
}

// TestReconcileAll_UnrecoverableRESTMappingFailureGetsPreCheckFailed proves a
// mapping error becomes PreCheckFailed without SSA.
func TestReconcileAll_UnrecoverableRESTMappingFailureGetsPreCheckFailed(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t)
	mapper := newUnrecoverableMappingMapper()
	store := memory.New()
	r := New(store, store, dyn, mapper, testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-no-mapping")
	content := newConfigMapContent(t, "cm-no-mapping", defaultNamespace, map[string]string{"k": "v"})
	seedApplyDesire(t, store, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil {
		t.Fatalf("status has no %q condition: %+v", desire.TypeSuccessful, got.Status)
	}
	if c.Status != metav1.ConditionFalse || c.Reason != desire.ReasonPreCheckFailed {
		t.Errorf("condition = %+v, want Status=False Reason=%q", c, desire.ReasonPreCheckFailed)
	}
	if !strings.Contains(c.Message, "no resource mapping") {
		t.Errorf("Message = %q, want it to mention the mapping failure", c.Message)
	}
	if patch := findPatchAction(dyn.Actions(), configMapGVR); patch != nil {
		t.Errorf("unrecoverable mapping failure should never reach the dynamic client, but got patch action: %+v", patch)
	}
}

// TestReconcileAll_ManifestMissingNamespaceFallsBackToIdentityNamespace
// proves that an omitted metadata.namespace is not an identity mismatch:
// apply uses d.Identity.Namespace. Every other namespaced fixture sets
// metadata.namespace explicitly, leaving this branch unexercised.
func TestReconcileAll_ManifestMissingNamespaceFallsBackToIdentityNamespace(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamicClient(t, newConfigMapObject("cm-no-ns-in-manifest", defaultNamespace))
	store := memory.New()
	r := New(store, store, dyn, newTestMapper(), testManagementCluster, time.Hour)

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-no-ns-in-manifest")
	content := newConfigMapContentNoNamespace(t, "cm-no-ns-in-manifest", map[string]string{"k": "v"})
	seedApplyDesire(t, store, id, "owner-1", content)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll() error = %v, want nil", err)
	}

	patch := findPatchAction(dyn.Actions(), configMapGVR)
	if patch == nil {
		t.Fatalf("no SSA apply patch action recorded; actions=%v", dyn.Actions())
	}
	if patch.GetNamespace() != defaultNamespace {
		t.Errorf(
			"patch namespace = %q, want %q (fallback to Identity.Namespace when the manifest omits it)",
			patch.GetNamespace(), defaultNamespace,
		)
	}

	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	c := findCondition(got.Status, desire.TypeSuccessful)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != desire.ReasonApplied {
		t.Errorf("condition = %+v, want Status=True Reason=%q", c, desire.ReasonApplied)
	}
}

// TestReconcileAll_SkipsApplyWhenDesireGoneBeforeApply proves applyToCluster's
// GetApplyDesire re-check prevents applying a desire that has already been
// removed from the SpecStore (e.g. superseded by a DeleteDesire) by the time
// the re-check runs, even though it was still present when ListApplyDesires
// listed it for this pass: no SSA patch is issued and no status is written.
func TestReconcileAll_SkipsApplyWhenDesireGoneBeforeApply(t *testing.T) {
	ctx := t.Context()
	dyn := newFakeDynamicClient(t) // no live object
	store := memory.New()

	id := applyIdentity("", "configmaps", defaultNamespace, "cm-gone")
	seedApplyDesire(t, store, id, "owner-1",
		newConfigMapContent(t, "cm-gone", defaultNamespace, map[string]string{"k": "v"}))

	r := New(
		&staleApplySpecStore{SpecStore: store, goneID: id},
		store, dyn, newTestMapper(), testManagementCluster, time.Hour,
	)

	if err := r.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}

	// No SSA patch may be issued for a desire gone from the store.
	if n := countApplyPatchActions(dyn.Actions(), configMapGVR); n != 0 {
		t.Errorf("apply patch actions = %d, want 0: a desire gone before apply must not be applied", n)
	}

	// And no status may be written for the skipped desire.
	got, err := store.GetApplyDesire(ctx, id)
	if err != nil {
		t.Fatalf("GetApplyDesire: %v", err)
	}
	if len(got.Status.Conditions) != 0 {
		t.Errorf("status conditions = %d, want 0: a skipped desire must not get status", len(got.Status.Conditions))
	}
}

// Ensures the fake dynamic client satisfies dynamic.Interface required by New.
var _ dynamic.Interface = &dynamicfake.FakeDynamicClient{}
