//go:build envtest

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

// identity builds an Identity for management cluster testManagementCluster,
// varying only what each test actually needs to vary.
func identity(dtype desire.DesireType, group, resource, namespace, name string) desire.Identity {
	return desire.Identity{
		ManagementCluster: testManagementCluster,
		Type:              dtype,
		Group:             group,
		Resource:          resource,
		Namespace:         namespace,
		Name:              name,
	}
}

func configMapIdentity(dtype desire.DesireType, name string) desire.Identity {
	return identity(dtype, "", "configmaps", defaultNamespace, name)
}

func clusterRoleIdentity(dtype desire.DesireType, name string) desire.Identity {
	return identity(dtype, rbacGroup, "clusterroles", "", name)
}

// widgetIdentity builds an Identity against gvr - each controller's
// CRD-reset test uses its own gvr (see installWidgetCRD) so their Widget
// CRDs never collide now that all three share one apiserver process.
func widgetIdentity(dtype desire.DesireType, gvr schema.GroupVersionResource, namespace, name string) desire.Identity {
	return identity(dtype, gvr.Group, gvr.Resource, namespace, name)
}

func newConfigMapContent(t *testing.T, name, namespace string, data map[string]string) json.RawMessage {
	t.Helper()
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"data": data,
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal configmap content: %v", err)
	}
	return raw
}

func newClusterRoleContent(t *testing.T, name string) json.RawMessage {
	t.Helper()
	return newClusterRoleContentWithNamespace(t, name, "")
}

// newClusterRoleContentWithNamespace optionally sets metadata.namespace, to
// exercise how a cluster-scoped apply behaves when the manifest names one
// anyway.
func newClusterRoleContentWithNamespace(t *testing.T, name, namespace string) json.RawMessage {
	t.Helper()
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	obj := map[string]any{
		"apiVersion": rbacGroup + "/v1",
		"kind":       "ClusterRole",
		"metadata":   metadata,
		"rules":      []any{},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal clusterrole content: %v", err)
	}
	return raw
}

func findCondition(status desire.Status, condType string) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

// waitForReason polls store for id's ReadDesire until its Successful
// condition's Reason matches want, or ctx's deadline is hit. Real watch
// delivery has real (if small) latency against envtest's apiserver, unlike
// the fakes the controllers' own unit tests use.
func waitForReason(
	t *testing.T, ctx context.Context, store desire.SpecStore, id desire.Identity, want string,
) desire.ReadDesire {
	t.Helper()
	var last desire.ReadDesire
	condition := func(ctx context.Context) (bool, error) {
		got, err := store.GetReadDesire(ctx, id)
		if err != nil {
			return false, err
		}
		last = got
		cond := findCondition(got.Status.Status, desire.TypeSuccessful)
		return cond != nil && cond.Reason == want, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, condition); err != nil {
		t.Fatalf("waiting for Reason=%q: %v (last status: %+v)", want, err, last.Status)
	}
	return last
}

// waitForApplyReason polls store for id's ApplyDesire until its Successful
// condition's Reason matches want, or ctx's deadline is hit. ApplyReconciler.Start
// reconciles on its own polling cadence rather than on a single caller-driven
// pass, so tests observe its effect by polling status instead of calling a
// synchronous reconcile method.
func waitForApplyReason(
	t *testing.T, ctx context.Context, store desire.SpecStore, id desire.Identity, want string,
) desire.ApplyDesire {
	t.Helper()
	var last desire.ApplyDesire
	condition := func(ctx context.Context) (bool, error) {
		got, err := store.GetApplyDesire(ctx, id)
		if err != nil {
			return false, err
		}
		last = got
		cond := findCondition(got.Status, desire.TypeSuccessful)
		return cond != nil && cond.Reason == want, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, condition); err != nil {
		t.Fatalf("waiting for Reason=%q: %v (last status: %+v)", want, err, last.Status)
	}
	return last
}

// waitForDeleteReason is waitForApplyReason's DeleteDesire counterpart -
// DeleteReconciler.Start is likewise a polling loop, not a single
// caller-driven pass.
func waitForDeleteReason(
	t *testing.T, ctx context.Context, store desire.SpecStore, id desire.Identity, want string,
) desire.DeleteDesire {
	t.Helper()
	var last desire.DeleteDesire
	condition := func(ctx context.Context) (bool, error) {
		got, err := store.GetDeleteDesire(ctx, id)
		if err != nil {
			return false, err
		}
		last = got
		cond := findCondition(got.Status, desire.TypeSuccessful)
		return cond != nil && cond.Reason == want, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, condition); err != nil {
		t.Fatalf("waiting for Reason=%q: %v (last status: %+v)", want, err, last.Status)
	}
	return last
}

// installWidgetCRD installs a "Widget" CRD at gvr/scope and registers a
// cleanup that removes it - shared by the three controllers' CRD-installed-
// after-mapper-cache-populated tests. Each caller passes its own gvr (a
// distinct API group per controller) so these can never collide now that
// all three share one apiserver process, regardless of run order.
func installWidgetCRD(t *testing.T, gvr schema.GroupVersionResource, scope apiextensionsv1.ResourceScope) {
	t.Helper()
	preserveUnknownFields := true
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   gvr.Resource,
				Singular: "widget",
				Kind:     "Widget",
				ListKind: "WidgetList",
			},
			Scope: scope,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    gvr.Version,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: &preserveUnknownFields,
					},
				},
			}},
		},
	}
	if _, err := envtest.InstallCRDs(envRESTConfig, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{crd},
	}); err != nil {
		t.Fatalf("InstallCRDs: %v", err)
	}
	t.Cleanup(func() {
		if err := envDynamicClient.Resource(schema.GroupVersionResource{
			Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
		}).Delete(context.Background(), crd.Name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("failed to clean up CRD %s: %v", crd.Name, err)
		}
	})
}

// cleanupNamespace deletes the named Namespace and clears its own
// spec.finalizers via the finalize subresource - envtest runs no namespace
// controller, so nothing else would ever complete the deletion and the name
// would stay stuck Terminating for the next test run.
func cleanupNamespace(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	if err := envK8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil &&
		!apierrors.IsNotFound(err) {
		t.Errorf("delete namespace %q: %v", name, err)
		return
	}
	var ns corev1.Namespace
	if err := envK8sClient.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Errorf("get namespace %q after delete: %v", name, err)
		return
	}
	ns.Spec.Finalizers = nil
	if err := envK8sClient.SubResource("finalize").Update(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("finalize namespace %q: %v", name, err)
	}
}

// removeFinalizer strips finalizer from the named Pod so its already
// in-flight deletion (deletionTimestamp already set) can complete.
func removeFinalizer(t *testing.T, namespace, name, finalizer string) {
	t.Helper()
	ctx := context.Background()
	var pod corev1.Pod
	if err := envK8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Errorf("get pod %s/%s: %v", namespace, name, err)
		return
	}
	finalizers := make([]string, 0, len(pod.Finalizers))
	for _, f := range pod.Finalizers {
		if f != finalizer {
			finalizers = append(finalizers, f)
		}
	}
	pod.Finalizers = finalizers
	if err := envK8sClient.Update(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("remove finalizer from pod %s/%s: %v", namespace, name, err)
	}
}

// createTarget creates obj via gvr/namespace on the live cluster and
// registers a t.Cleanup that deletes it, ignoring NotFound. namespace is
// empty for cluster-scoped resources.
func createTarget(
	t *testing.T, ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured,
) *unstructured.Unstructured {
	t.Helper()
	var ri dynamic.ResourceInterface = envDynamicClient.Resource(gvr)
	if namespace != "" {
		ri = envDynamicClient.Resource(gvr).Namespace(namespace)
	}
	created, err := ri.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create %s %q: %v", gvr.Resource, obj.GetName(), err)
	}
	t.Cleanup(func() {
		if err := ri.Delete(context.Background(), obj.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete %s %q: %v", gvr.Resource, obj.GetName(), err)
		}
	})
	return created
}
