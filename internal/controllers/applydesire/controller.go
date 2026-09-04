// Package applydesire reconciles ApplyDesires against the local kube-apiserver.
// A Reconciler is bound to one management cluster and applies listed desires
// via SSA, recording the result as status.
package applydesire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	hflog "github.com/openshift-hyperfleet/hyperfleet-logger"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/openshift-hyperfleet/hyperfleet-applier/internal/controllers/util"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
)

// fieldManager is the single global SSA field manager for the applier,
// per the single-writer ownership model.
const fieldManager = "hyperfleet-applier"

const (
	// applyTimeout bounds a single SSA Apply call so a hung apiserver
	// cannot stall an entire reconcileAll pass indefinitely.
	applyTimeout = 30 * time.Second
)

// specLister is the read-only SpecStore surface the reconciler needs.
type specLister interface {
	ListApplyDesires(ctx context.Context, managementCluster string) ([]desire.ApplyDesire, error)
	GetApplyDesire(ctx context.Context, id desire.Identity) (desire.ApplyDesire, error)
}

type statusWriter interface {
	UpdateApplyDesireStatus(
		ctx context.Context, id desire.Identity, status desire.Status, version int64,
	) (desire.ApplyDesire, error)
}

type ApplyReconciler struct {
	spec              specLister
	status            statusWriter
	dyn               dynamic.Interface
	mapper            meta.RESTMapper
	managementCluster string
	applyTimeout      time.Duration
	pollInterval      time.Duration
}

// New builds a ApplyReconciler for one management cluster.
// mapper resolves GVKs for SSA; discovery cache ownership stays with the host.
func New(
	spec specLister,
	status statusWriter,
	dyn dynamic.Interface,
	mapper meta.RESTMapper,
	managementCluster string,
	pollInterval time.Duration,
) *ApplyReconciler {
	return &ApplyReconciler{
		spec:              spec,
		status:            status,
		dyn:               dyn,
		mapper:            mapper,
		managementCluster: managementCluster,
		applyTimeout:      applyTimeout,
		pollInterval:      pollInterval,
	}
}

// Start reconciles immediately, then repeats at the fixed polling cadence
// until ctx is canceled. Reconciliation failures are logged and retried on the
// next pass; caller-driven shutdown returns nil.
func (r *ApplyReconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		if err := r.reconcileAll(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "apply: reconciliation pass failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// reconcileAll lists every ApplyDesire in the partition and reconciles each.
// Ordinary apply failures are recorded in status and excluded from the returned
// error; only non-conflict status-write failures are joined. Context
// cancellation aborts immediately and is not written as status.
func (r *ApplyReconciler) reconcileAll(ctx context.Context) error {
	desires, err := r.spec.ListApplyDesires(ctx, r.managementCluster)
	if err != nil {
		return fmt.Errorf("apply: list apply desires for management cluster %q: %w", r.managementCluster, err)
	}

	var errs []error
	for _, d := range desires {
		// Do not record shutdown as a per-desire failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("apply: reconcile aborted for management cluster %q: %w", r.managementCluster, ctxErr)
		}
		// Handles deadlineExceeded and context canceled
		if reconcileErr := r.reconcileOne(ctx, d); reconcileErr != nil {
			if errors.Is(reconcileErr, context.Canceled) || errors.Is(reconcileErr, context.DeadlineExceeded) {
				return fmt.Errorf(
					"apply: reconcile desire %s: %w",
					util.DescribeIdentity(d.Identity), reconcileErr,
				)
			}
			// WithResourceID carries a display-only identity string.
			logCtx := hflog.WithResourceType(ctx, "apply_desire")
			logCtx = hflog.WithResourceID(logCtx, util.DescribeIdentity(d.Identity))
			slog.ErrorContext(logCtx, "apply: reconcile failed",
				"identity", d.Identity,
				"error", reconcileErr,
			)
			errs = append(errs, reconcileErr)
		}
	}
	return errors.Join(errs...)
}

// reconcileOne parses d, resolves its target, applies it via SSA, and writes
// the resulting status. It never mutates the SpecStore.
func (r *ApplyReconciler) reconcileOne(ctx context.Context, d desire.ApplyDesire) error {
	newStatus, err := r.applyToCluster(ctx, d)
	if err != nil {
		if errors.Is(err, desire.ErrNotFound) {
			// The ApplyDesire was removed (e.g. superseded by a DeleteDesire)
			// between listing and applying; nothing to apply or record.
			slog.DebugContext(ctx, "apply: desire deleted before apply, skipping",
				"identity", d.Identity,
			)
			return nil
		}
		// Cancellation aborts the pass instead of writing status.
		return err
	}

	if util.Equal(newStatus, d.Status) {
		return nil
	}

	if _, err := r.status.UpdateApplyDesireStatus(ctx, d.Identity, newStatus, d.Version); err != nil {
		if errors.Is(err, desire.ErrVersionConflict) {
			// Benign race; the next poll retries with the fresh version.
			slog.DebugContext(ctx, "apply: status write lost a version race, will retry next pass",
				"identity", d.Identity,
			)
			return nil
		}
		return fmt.Errorf("apply: write status for %s: %w", util.DescribeIdentity(d.Identity), err)
	}
	return nil
}

// applyToCluster returns the status to persist and, separately, an error.
// Context cancellation and GetApplyDesire-returned errors (the desire vanished from the
// SpecStore before it could be applied) return a non-nil
// error for reconcileOne to handle as control flow; all other outcomes are encoded as
// status conditions.
func (r *ApplyReconciler) applyToCluster(ctx context.Context, d desire.ApplyDesire) (desire.Status, error) {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(d.Spec.KubeContent, obj); err != nil {
		return preCheckFailed(d.Status, fmt.Sprintf(
			"apply: manifest could not be decoded as a Kubernetes object (invalid JSON or missing kind): %v", err,
		)), nil
	}
	if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
		return preCheckFailed(d.Status, "apply: manifest is missing apiVersion, kind, or metadata.name"), nil
	}

	gvk := obj.GroupVersionKind()
	mapping, err := r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil && meta.IsNoMatchError(err) {
		// The resource may have just been installed (e.g. a new CRD) after
		// the mapper's discovery cache was already populated - reset and
		// retry once before giving up.
		if resettable, ok := r.mapper.(meta.ResettableRESTMapper); ok {
			resettable.Reset()
		}
		mapping, err = r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return preCheckFailed(d.Status, fmt.Sprintf("apply: no resource mapping for %s: %v", gvk, err)), nil
	}
	if err := checkApplyTarget(d.Identity, obj, mapping); err != nil {
		return preCheckFailed(d.Status, err.Error()), nil
	}

	ri := r.dyn.Resource(mapping.Resource)
	var resourceClient dynamic.ResourceInterface = ri
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resourceClient = ri.Namespace(d.Identity.Namespace)
	}

	// Re-check the desire still exists immediately before applying, to shrink
	// the race window against a concurrent DeleteDesire removing it from the
	// SpecStore after this desire was listed
	if _, err := r.spec.GetApplyDesire(ctx, d.Identity); err != nil {
		if errors.Is(err, desire.ErrNotFound) {
			return desire.Status{}, fmt.Errorf(
				"apply: desire %s deleted before apply: %w", util.DescribeIdentity(d.Identity), err,
			)
		}
		return desire.Status{}, fmt.Errorf(
			"apply: verify desire %s still exists: %w", util.DescribeIdentity(d.Identity), err,
		)
	}

	applyCtx, cancel := context.WithTimeout(ctx, r.applyTimeout)
	defer cancel()
	if _, err := resourceClient.Apply(applyCtx, d.Identity.Name, obj, metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Abort instead of recording shutdown as KubeAPIError.
			return desire.Status{}, fmt.Errorf("apply %s: %w", util.DescribeIdentity(d.Identity), ctxErr)
		}
		return applyFailed(d.Status, err), nil
	}
	return applied(d.Status), nil
}

// checkApplyTarget rejects manifests that target a different object than id.
// An omitted manifest namespace is allowed; apply uses Identity.Namespace.
func checkApplyTarget(id desire.Identity, obj *unstructured.Unstructured, mapping *meta.RESTMapping) error {
	gvr := mapping.Resource
	if gvr.Group != id.Group || gvr.Resource != id.Resource {
		return fmt.Errorf(
			"apply: manifest resource %q/%q does not match identity %q/%q",
			gvr.Group, gvr.Resource, id.Group, id.Resource,
		)
	}
	if obj.GetName() != id.Name {
		return fmt.Errorf("apply: manifest name %q does not match identity name %q", obj.GetName(), id.Name)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if id.Namespace == "" {
			return fmt.Errorf(
				"apply: identity namespace must not be empty for namespaced resource %q", id.Name,
			)
		}
		if ns := obj.GetNamespace(); ns != "" && ns != id.Namespace {
			return fmt.Errorf(
				"apply: manifest namespace %q does not match identity namespace %q", ns, id.Namespace,
			)
		}
	} else if id.Namespace != "" {
		// Require one identity per cluster-scoped object.
		return fmt.Errorf(
			"apply: identity namespace %q must be empty for cluster-scoped resource %q", id.Namespace, id.Name,
		)
	}
	return nil
}
