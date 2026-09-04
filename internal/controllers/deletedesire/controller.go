// Package deletedesire reconciles DeleteDesires against the local kube-apiserver.
//
// A DeleteReconciler is bound to one management-cluster partition. Each reconcileAll pass
// lists DeleteDesires from the spec store and reconciles each by deleting the resource
// and confirming its absence past finalizers.
package deletedesire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	hflog "github.com/openshift-hyperfleet/hyperfleet-logger"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/openshift-hyperfleet/hyperfleet-applier/internal/controllers/util"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// deleteTimeout bounds a single Delete call so a hung apiserver
	// cannot stall an entire reconcileAll pass indefinitely.
	deleteTimeout = 30 * time.Second
)

// specLister is the read side of the SpecStore.
// Declaring the narrow interface documents the real dependency and keeps test fakes small.
type specLister interface {
	ListDeleteDesires(ctx context.Context, managementCluster string) ([]desire.DeleteDesire, error)
	GetDeleteDesire(ctx context.Context, id desire.Identity) (desire.DeleteDesire, error)
}

// statusWriter updates desire status.
type statusWriter interface {
	UpdateDeleteDesireStatus(
		ctx context.Context, id desire.Identity, status desire.Status, version int64,
	) (desire.DeleteDesire, error)
}

// DeleteReconciler reconciles DeleteDesires by deleting resources and confirming absence.
type DeleteReconciler struct {
	spec              specLister
	status            statusWriter
	dyn               dynamic.Interface
	mapper            meta.RESTMapper
	managementCluster string
	timeout           time.Duration
	pollInterval      time.Duration
}

// New creates a new DeleteReconciler.
func New(
	spec specLister,
	status statusWriter,
	dyn dynamic.Interface,
	mapper meta.RESTMapper,
	managementCluster string,
	pollInterval time.Duration,
) *DeleteReconciler {
	return &DeleteReconciler{
		spec:              spec,
		status:            status,
		dyn:               dyn,
		mapper:            mapper,
		managementCluster: managementCluster,
		timeout:           deleteTimeout,
		pollInterval:      pollInterval,
	}
}

// Start reconciles immediately, then repeats at the fixed polling cadence
// until ctx is canceled. Reconciliation failures are logged and retried on the
// next pass; caller-driven shutdown returns nil.
func (r *DeleteReconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		if err := r.reconcileAll(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "delete: reconciliation pass failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// reconcileAll lists every DeleteDesire in the partition and reconciles each.
// A failure on one desire is recorded on that desire's status and does not abort
// the others; every such failure is also joined into the returned error so the
// host can drive retry/backoff and surface controller health. The error is nil
// only when the list succeeds and no desire failed.
//
// Context cancellation is treated differently: it is caller-driven control flow
// (e.g. shutdown), not a resource failure, so it aborts the pass immediately
// and is returned without being recorded on any desire's status.
func (r *DeleteReconciler) reconcileAll(ctx context.Context) error {
	desires, err := r.spec.ListDeleteDesires(ctx, r.managementCluster)
	if err != nil {
		return fmt.Errorf("delete: list desires for partition %q: %w", r.managementCluster, err)
	}

	var errs []error
	for _, d := range desires {
		// Stop promptly on cancellation instead of recording shutdown as a
		// per-desire failure across every remaining desire.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("delete: reconcile aborted for partition %q: %w", r.managementCluster, ctxErr)
		}
		if reconcileErr := r.reconcileOne(ctx, d); reconcileErr != nil {
			// Handles deadlineExceeded and context canceled
			if ctx.Err() != nil {
				return reconcileErr
			}
			logCtx := hflog.WithResourceType(ctx, "delete_desire")
			logCtx = hflog.WithResourceID(logCtx, util.DescribeIdentity(d.Identity))
			slog.ErrorContext(logCtx, "delete: reconcile failed",
				"identity", d.Identity,
				"error", reconcileErr,
			)
			errs = append(errs, reconcileErr)
		}
	}
	return errors.Join(errs...)
}

// reconcileOne deletes a resource from the cluster and writes the resulting
// condition back to the status store. It never mutates the SpecStore.
//
// ReasonDeleted means the resource is confirmed absent (404 on GET or successful
// delete with no finalizers). ReasonWaitingForDeletion means the delete was
// issued but the resource still exists (finalizers, graceful termination).
func (r *DeleteReconciler) reconcileOne(ctx context.Context, d desire.DeleteDesire) error {
	var newStatus desire.Status

	// Step 1: Pre-flight checks - resolve GVR, validate namespace
	client, err := r.setupResourceClient(d.Identity)
	if err != nil {
		newStatus = preCheckFailed(d.Status, err.Error())
	} else {
		// Re-check the desire hasn't already succeeded before deleting, to
		// shrink the race window against a concurrent update completing the
		// deletion before this pass executes.
		dd, err := r.spec.GetDeleteDesire(ctx, d.Identity)
		if err != nil {
			return fmt.Errorf(
				"delete: re-check desire %s: %w", util.DescribeIdentity(d.Identity), err,
			)
		}
		if desire.IsDeleted(dd.Status) {
			// Already completed successfully on a prior pass, nothing more to do.
			slog.DebugContext(ctx, "delete: desire already completed, skipping",
				"identity", d.Identity,
			)
			return nil
		}

		// Step 2: Execute the actual delete operation (GET → DELETE → GET)
		newStatus, err = r.executeDelete(ctx, client, d)
		if err != nil {
			// Context cancellation - abort without writing status.
			return err
		}
	}
	// Step 3: Check to see if newStatus and old status are equal
	if util.Equal(newStatus, d.Status) {
		return nil
	}

	// Step 4: Update DeleteDesire status with newStatus - will abort if context is canceled
	if _, err := r.status.UpdateDeleteDesireStatus(ctx, d.Identity, newStatus, d.Version); err != nil {
		if errors.Is(err, desire.ErrVersionConflict) {
			slog.DebugContext(ctx, "delete: status write lost a version race, will retry next pass",
				"identity", d.Identity,
			)
			return nil
		}
		if ctx.Err() != nil {
			// Context canceled during status write - abort
			return ctx.Err()
		}
		return fmt.Errorf("delete: write status for %s: %w", util.DescribeIdentity(d.Identity), err)
	}
	return nil
}

// setupResourceClient resolves the GVR mapping and returns a namespace-scoped or
// cluster-scoped resource client. Returns error for mapping failures or invalid
// namespace configuration.
func (r *DeleteReconciler) setupResourceClient(id desire.Identity) (dynamic.ResourceInterface, error) {
	partialGVR := schema.GroupVersionResource{
		Group:    id.Group,
		Resource: id.Resource,
	}

	// KindFor returns the Kind corresponding to the GVR.
	gvk, err := r.mapper.KindFor(partialGVR)
	if err != nil {
		if meta.IsNoMatchError(err) {
			// Reset mapper cache and retry once - the resource may have been
			// uninstalled or this is the first lookup after CRD installation.
			if resettable, ok := r.mapper.(meta.ResettableRESTMapper); ok {
				resettable.Reset()
			}
			gvk, err = r.mapper.KindFor(partialGVR)

		}
		if err != nil {
			return nil, err
		}
	}

	// Get the full mapping information (includes scope).
	mapping, err := r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve REST mapping: %w", err)
	}

	// Configure the resource client for either namespace-scoped or cluster-scoped access.
	// Kubernetes resources fall into two categories:
	//   - Namespace-scoped (e.g., Pods, ConfigMaps): require .Namespace(ns).Delete(name)
	//   - Cluster-scoped (e.g., Nodes, ClusterRoles): require .Delete(name) only
	// The mapper tells us which scope applies to this resource type.
	ri := r.dyn.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if id.Namespace == "" {
			return nil, fmt.Errorf(
				"delete: identity namespace must not be empty for namespaced resource %q", id.Name,
			)
		}
		return ri.Namespace(id.Namespace), nil
	}
	return ri, nil
}

// getResourceIfExist performs a GET with timeout and returns whether the resource
// exists. Returns (true, obj, nil) if found, (false, nil, nil) if NotFound, or
// (false, nil, err) on API errors.
func (r *DeleteReconciler) getResourceIfExist(
	ctx context.Context, client dynamic.ResourceInterface, name string,
) (bool, *unstructured.Unstructured, error) {
	getCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	obj, err := client.Get(getCtx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, obj, nil
}

// executeDelete performs the GET → check deletion timestamp → DELETE (if needed) → GET flow.
// Successful deletion is only reported when the final GET returns NotFound.
func (r *DeleteReconciler) executeDelete(
	ctx context.Context, client dynamic.ResourceInterface, d desire.DeleteDesire,
) (desire.Status, error) {
	// First, check if the resource exists and get its current state.
	exists, obj, err := r.getResourceIfExist(ctx, client, d.Identity.Name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return desire.Status{}, fmt.Errorf("delete %s: %w", util.DescribeIdentity(d.Identity), ctxErr)
		}
		return deleteFailed(d.Status, fmt.Errorf("failed to check existence: %w", err)), nil
	}
	if !exists {
		// Resource is already gone - success.
		return deleted(d.Status), nil
	}
	deletionTimestamp := obj.GetDeletionTimestamp()
	// Resource exists - check if deletion is already in progress.
	if deletionTimestamp != nil {
		// Deletion already in progress (from previous reconcile or manual kubectl delete).
		// Object still exists, waiting for finalizers to complete.
		uid := obj.GetUID()
		msg := fmt.Sprintf("deletion timestamp: %s, uid: %s", deletionTimestamp.Format(time.RFC3339), uid)
		return waitingForDeletion(d.Status, msg), nil
	}

	// No deletion timestamp yet - issue the delete call.
	deleteCtx, deleteCancel := context.WithTimeout(ctx, r.timeout)
	defer deleteCancel()

	err = client.Delete(deleteCtx, d.Identity.Name, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted between GET and DELETE - success.
			return deleted(d.Status), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return desire.Status{}, fmt.Errorf("delete %s: %w", util.DescribeIdentity(d.Identity), ctxErr)
		}
		return deleteFailed(d.Status, err), nil
	}

	// Delete was accepted. Check if the object is actually gone now.
	exists, obj, err = r.getResourceIfExist(ctx, client, d.Identity.Name)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return desire.Status{}, fmt.Errorf("delete %s: %w", util.DescribeIdentity(d.Identity), ctxErr)
		}
		return deleteFailed(d.Status, fmt.Errorf("failed to confirm deletion: %w", err)), nil
	}
	if !exists {
		// Confirmed gone - success.
		return deleted(d.Status), nil
	}

	// Object still exists after delete - it has finalizers or is in graceful termination.
	// Extract deletion timestamp and UID for the status message.
	deletionTimestamp = obj.GetDeletionTimestamp()
	if deletionTimestamp == nil {
		// Object exists but has no deletion timestamp - unexpected state.
		return deleteFailed(d.Status, fmt.Errorf("delete accepted but object has no deletion timestamp")), nil
	}
	uid := obj.GetUID()
	msg := fmt.Sprintf("deletion timestamp: %s, uid: %s", deletionTimestamp.Format(time.RFC3339), uid)
	return waitingForDeletion(d.Status, msg), nil
}
