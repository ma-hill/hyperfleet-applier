# internal/controllers/deletedesire

`DeleteReconciler` reconciles DeleteDesires (see `pkg/desire/CLAUDE.md`) against the local kube-apiserver
via the DELETE API and confirmation checks.

`DeleteReconciler` is bound to one management-cluster partition. `Start` calls private
`reconcileAll` immediately, then at its host-configured interval, and returns nil when its context is canceled.
Each pass lists every DeleteDesire for that partition and reconciles each independently - one failure is recorded on that desire's
status and does not abort the others, but every such failure is `errors.Join`-ed into the value
`reconcileAll` returns, so the host can drive retry/backoff and expose controller health. The return
is nil only when the list succeeds and no desire failed. `reconcileAll` depends on narrow unexported
interfaces (`specLister.ListDeleteDesires`, `statusWriter.UpdateDeleteDesireStatus`), not the full
`SpecStore`/`StatusStore`.

**Context cancellation** is handled apart from resource failures: it is caller-driven control flow
(e.g. shutdown), not evidence the resource failed. `reconcileAll` checks `ctx.Err()` before each
desire, and `executeDelete` returns the context error (rather than a `KubeAPIError` status) when a
DELETE call fails with `ctx.Err() != nil`. Either way the pass aborts immediately and returns the
context error without recording any status, so healthy statuses are never overwritten during
shutdown.

Per desire, `reconcileOne` performs a three-phase execution:

1. **Pre-flight checks** (`setupResourceClient`):
   - Resolve GVK → GVR via the injected `meta.RESTMapper`. For `NoMatchError` (e.g., CRD
     absent), the controller resets the mapper and retries once. Any failure from
     `setupResourceClient` - a `NoMatchError` that survives the retry, an invalid namespace, or any
     other mapping error - is a precheck failure (`ReasonPreCheckFailed`), matching applydesire's and
     readdesire's identical policy.

2. **Execute delete** (`executeDelete` - GET → DELETE → GET flow):
   - **First GET:** Check if the resource exists. If `NotFound`, it's already deleted →
     `ReasonDeleted` (success).
   - **DELETE:** Issue the delete call with a 30-second timeout. If `NotFound` is returned (already
     deleted between GET and DELETE), treat as success → `ReasonDeleted`.
   - **Second GET:** Confirm deletion. If `NotFound`, deletion complete → `ReasonDeleted`. If the
     resource still exists, it has finalizers or is in graceful termination →
     `ReasonWaitingForDeletion` with deletion timestamp and UID in the message.
   - Kubernetes API errors during any step → `ReasonKubeAPIError`.

3. **Write status:**
   - Write the resulting condition back through `StatusStore.UpdateDeleteDesireStatus`, using the
     desire's `Version` read at list time. A `ErrVersionConflict` here means spec/status moved since
     `ListDeleteDesires` - treated as a benign race, not an error; the next `reconcileAll` pass retries.
   - `util.Equal` (ignoring `LastTransitionTime`) suppresses status writes that wouldn't change
     anything.

The reconciler reads DeleteDesire intent and writes reconciliation status. It does not mutate
desire intent through the spec store. Authentication and storage-level authorization are outside
the controller's responsibility; the narrow `specLister`/`statusWriter` interfaces constrain
normal Go callers only.

## Status Conditions

- **`ReasonDeleted`** (`Successful=True`): Resource confirmed absent (404 on GET or successful
  deletion with no finalizers blocking).
- **`ReasonWaitingForDeletion`** (`Successful=False`): DELETE was accepted, but the resource still
  exists (finalizers or graceful termination). Message includes deletion timestamp and UID per
  acceptance criteria.
- **`ReasonPreCheckFailed`** (`Successful=False`): Pre-flight validation failed (mapping error,
  invalid namespace for namespaced resource). No kube-apiserver call attempted.
- **`ReasonKubeAPIError`** (`Successful=False`): Kubernetes API error during GET/DELETE operations.

## Special Cases

- **NoMatchError (CRD absent):**  This is treated is surfaced as `ReasonPreCheckFailed`
- **Namespace-scoped vs cluster-scoped:** The controller configures the dynamic client correctly
  based on the REST mapper's scope information. Namespace-scoped resources (e.g., Pods) require
  `.Namespace(ns).Delete(name)`, while cluster-scoped (e.g., ClusterRoles) use `.Delete(name)` only.
- **Finalizers:** Resources with finalizers enter `WaitingForDeletion` state. The controller does
  not remove finalizers - that's the responsibility of the finalizer owner. Subsequent reconcile
  passes continue checking until the resource is fully deleted.

## Operational semantics (MVP)

- **Polling only:** `Start` provides the finite reconciliation cadence; `reconcileAll` has no
  internal workqueue, rate limiter, or per-desire backoff. The hosting binary must still configure
  client-side QPS/burst so unchanged desires cannot generate unbounded DELETE traffic.
- **Cost per tick:** every listed desire gets a full GET → DELETE → GET round-trip each pass, even
  when the resource is already deleted. Already-deleted desires suppress the status-store write
  (status unchanged), but not the apiserver call.
- **`ReasonDeleted` meaning:** success is recorded when the kube-apiserver confirms the resource is
  absent (404 on final GET). The controller does not wait for finalizers to complete - that results
  in `WaitingForDeletion`.
- **Delete before status CAS:** DELETE runs before `UpdateDeleteDesireStatus`. If the status write
  loses a version race, the cluster may have deleted the resource while the store hasn't recorded
  it; the next pass confirms deletion. The per-resource `Version` is shared across Apply/Delete/Read
  sub-states for the same Kubernetes target, so unrelated store writes can also bump it and trigger
  benign `ErrVersionConflict` on the delete status path.
- **Timeouts:** Each GET and DELETE call has a 30-second timeout (`defaultDeleteTimeout`) to prevent
  hung apiserver connections from stalling an entire reconciliation pass.

