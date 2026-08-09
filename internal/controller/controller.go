package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/bootstrap"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
	"github.com/kubeswift-io/gpucellpool/internal/cellid"
	"github.com/kubeswift-io/gpucellpool/internal/inventory"
	poolmetrics "github.com/kubeswift-io/gpucellpool/internal/metrics"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
	"github.com/kubeswift-io/gpucellpool/internal/workload"
)

const (
	requeueProgressing = 30 * time.Second
	requeueSteady      = 2 * time.Minute
)

// GPUCellPoolReconciler reconciles GPUCellPool objects across two clusters: it
// owns cells in the infrastructure cluster and observes their Nodes and GPU
// capacity in the workload cluster.
type GPUCellPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Clients  *workload.ClientCache

	// Clock and the two factories are seams for tests.
	Clock          func() time.Time
	NewProvisioner func(client.Client) provisioner.Provisioner
	NewProvider    func(kubernetes.Interface, string) capacity.Provider
}

// +kubebuilder:rbac:groups=cells.kubeswift.io,resources=gpucellpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cells.kubeswift.io,resources=gpucellpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cells.kubeswift.io,resources=gpucellpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=swift.kubeswift.io,resources=swiftguests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=seed.kubeswift.io,resources=swiftseedprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices;resourceclaims;resourceclaimtemplates;deviceclasses,verbs=get;list;watch
// ClusterAPI provisioner. The bootstrap groups are wildcarded because the
// bootstrap provider is the user's choice (kubeadm, k0smotron, ...) and its kind
// is named in the pool spec, not known at build time.
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubeswiftmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// Secrets need create+update, not only read: the operator RENDERS a per-cell
// bootstrap Secret from the user's join template (reconcileBootstrapSecret). A
// read-only grant let the chart install, pass every test that used admin
// credentials, and then fail on the first cell with "secrets is forbidden".
// Delete is deliberately absent — the per-cell Secret carries the pool's
// ownerRef and is garbage-collected.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update
// Events need BOTH groups. The core-group grant alone looks right and is
// useless: the recorder writes through the events.k8s.io API, so every event
// this operator emits was rejected with "events.events.k8s.io is forbidden" —
// visible only in the manager log, never to the operator reading `kubectl
// describe gpucellpool`. The core grant stays for clients that still read there.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile implements the loop in docs/design/gpucellpool-reconciliation.md §4.
func (r *GPUCellPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pool cellsv1alpha1.GPUCellPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	prov, err := r.provisioner(&pool)
	if err != nil {
		// Nothing can be created safely, and the operator must be told rather than
		// left watching a pool that never does anything.
		r.event(&pool, corev1.EventTypeWarning, cellsv1alpha1.ReasonUnknownProvisioner, err.Error())
		return ctrl.Result{}, err
	}

	if !pool.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &pool, prov)
	}

	if controllerutil.AddFinalizer(&pool, cellsv1alpha1.FinalizerPool) {
		if err := r.Update(ctx, &pool); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding pool finalizer: %w", err)
		}
	}

	// Step 3: the workload cluster. A failure here is first-class state, not an
	// error return: it freezes every destructive path below.
	inner, reachable, reachReason, reachMsg := r.workloadClient(ctx, &pool)

	var provider capacity.Provider
	if reachable {
		provider = r.provider(inner, hamiMode(&pool))
	}

	// Step 4: rediscover cells from live objects. status.cells is a projection,
	// so a restart loses nothing.
	cells, err := r.discoverCells(ctx, &pool, prov, inner, provider, reachable)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 10 (partly): the outer inventory, which also gates creation below.
	// A read failure, or a driver that publishes nothing at all, both leave this
	// nil: UNKNOWN. Only a driver that publishes devices we can see is allowed to
	// stall the pool with "no free GPU".
	var freeGPUs *int
	if counts, invErr := inventory.FreeGPUs(ctx, r.Client, ""); invErr != nil {
		log.V(1).Info("physical inventory unreadable; treating free GPUs as unknown", "err", invErr.Error())
	} else if counts.Known {
		freeGPUs = &counts.Free
	}

	// Capacity and demand BEFORE membership: the scaling decision needs both.
	var (
		cap0          capacity.Capacity
		capacityKnown bool
		health        capacity.Health
		demand        capacity.Demand
		demandKnown   bool
		shape         *capacity.Device
	)
	if reachable && provider != nil {
		nodes := readyNodeNames(cells)
		if h, hErr := provider.Health(ctx, nodes); hErr != nil {
			health = capacity.Health{Reason: cellsv1alpha1.ReasonHAMiNotDetected, Message: hErr.Error()}
		} else {
			health = h
		}
		if c, cErr := provider.Capacity(ctx, nodes); cErr != nil {
			log.V(1).Info("capacity unreadable; retaining the last known values", "err", cErr.Error())
			poolmetrics.CapacityScrapeErrorsTotal.WithLabelValues(pool.Name, pool.Namespace, "capacity").Inc()
		} else {
			cap0, capacityKnown = c, true
		}
		if autoscalingEnabled(&pool) {
			// The reference device is what a fresh cell would bring: what the pool
			// advertises now, else the shape it remembers from when it last had a
			// cell. Nil means we cannot judge satisfiability at all, and the scaler
			// then refuses to act rather than guessing.
			shape = CellShape(cap0, pool.Status.CellDeviceShape)
			if d, dErr := provider.PendingDemand(ctx, shape); dErr != nil {
				log.V(1).Info("GPU demand unreadable; not scaling", "err", dErr.Error())
				poolmetrics.CapacityScrapeErrorsTotal.WithLabelValues(pool.Name, pool.Namespace, "demand").Inc()
			} else {
				demand, demandKnown = d, true
			}
		}
	}

	// Remember the shape whenever a live cell shows it. This outlives the cells so a
	// pool that scaled to zero can still tell whether a fresh one would help.
	if learned := RememberShape(ReferenceDevice(cap0), r.now()); learned != nil {
		pool.Status.CellDeviceShape = learned
	}

	poolmetrics.WorkloadClusterReachable.WithLabelValues(pool.Name, pool.Namespace).
		Set(boolGauge(reachable))

	// Idleness is read from the capacity provider, not guessed: only a cell that
	// holds nothing may be removed automatically.
	var idle []string
	if needsIdleness(&pool) && reachable && provider != nil {
		idle = r.idleCells(ctx, provider, cells)
	}

	scale := DecideScale(ScaleInput{
		Replicas:        pool.Spec.Replicas,
		Autoscaling:     pool.Spec.Autoscaling,
		LiveCells:       liveCells(cells),
		Demand:          demand,
		DemandKnown:     demandKnown,
		ShapeKnown:      shape != nil,
		FreeGPUs:        freeGPUs,
		IdleCells:       idle,
		Now:             r.now(),
		LastScaleUp:     pool.Status.LastScaleUpTime,
		LastScaleDown:   pool.Status.LastScaleDownTime,
		DemandFreeSince: pool.Status.DemandFreeSince,
	})
	if autoscalingEnabled(&pool) {
		poolmetrics.ScaleDecisionsTotal.WithLabelValues(
			pool.Name, pool.Namespace, scale.Reason, strconv.FormatBool(scale.ScaledUp)).Inc()
	}
	// DemandFreeSince is what makes "demand has been absent for the whole window"
	// answerable; demand being absent right now is not the same thing.
	if demandKnown {
		if demand.SatisfiableByOneCell == 0 && demand.PendingRequests == 0 {
			if pool.Status.DemandFreeSince == nil {
				t := metav1.NewTime(r.now())
				pool.Status.DemandFreeSince = &t
			}
		} else {
			pool.Status.DemandFreeSince = nil
		}
	}
	if scale.ScaledUp {
		nowT := metav1.NewTime(r.now())
		pool.Status.LastScaleUpTime = &nowT
		r.event(&pool, corev1.EventTypeNormal, scale.Reason, scale.Message)
	}
	if scale.ScaledDown {
		nowT := metav1.NewTime(r.now())
		pool.Status.LastScaleDownTime = &nowT
		r.event(&pool, corev1.EventTypeNormal, scale.Reason, scale.Message)
	}

	// Steps 5-6: membership.
	plan := PlanMembership(MembershipInput{
		Desired:           scale.Desired,
		Cells:             cells,
		Now:               r.now(),
		FreeGPUs:          freeGPUs,
		WorkloadReachable: reachable,
		EverReady:         everReady(&pool, cells),
		DrainPreference:   scale.DrainCandidates,
	})

	for _, idx := range plan.Create {
		if err := r.createCell(ctx, &pool, prov, idx); err != nil {
			return ctrl.Result{}, err
		}
		r.event(&pool, corev1.EventTypeNormal, cellsv1alpha1.ReasonCellCreating,
			fmt.Sprintf("creating cell %s", cellid.Name(pool.Name, idx)))
	}
	if len(plan.Create) > 0 {
		// Re-observe so this pass's status already mentions the cells it just
		// created. Otherwise `kubectl get` right after `apply` reports zero cells
		// while their guests plainly exist, which reads as a broken pool.
		if cells, err = r.discoverCells(ctx, &pool, prov, inner, provider, reachable); err != nil {
			return ctrl.Result{}, err
		}
	}
	for _, name := range plan.Drain {
		r.markDraining(cells, name)
		r.event(&pool, corev1.EventTypeNormal, cellsv1alpha1.ReasonCellDraining,
			fmt.Sprintf("draining cell %s", name))
	}

	// Replace failed cells whose backoff has expired: delete the guest and let
	// the next pass recreate the index.
	for i := range cells {
		if ShouldReplace(cells[i], r.now()) {
			if err := r.deleteCell(ctx, &pool, prov, cells[i], false); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// A template change replaces cells only when asked to, and only through the same
	// drain gate scale-down uses. Deliberately after the membership plan: a rollout
	// must not race a scale decision for the index it is about to free.
	rollout := PlanRollout(RolloutInput{
		Policy:            pool.Spec.UpdatePolicy,
		DesiredHash:       cellid.TemplateHash(pool.Spec.Cell.GuestTemplate.Raw),
		Cells:             cells,
		IdleCells:         idle,
		WorkloadReachable: reachable,
		AtDesiredSize:     liveCells(cells) == scale.Desired && len(plan.Create) == 0,
	})
	if rollout.Drain != "" {
		r.markDraining(cells, rollout.Drain)
		r.event(&pool, corev1.EventTypeNormal, rollout.Reason, rollout.Message)
	}

	// Draining cells: gate on allocations, then remove.
	for i := range cells {
		if cells[i].Phase != cellsv1alpha1.CellPhaseDraining {
			continue
		}
		if err := r.progressDrain(ctx, &pool, prov, provider, inner, &cells[i], false); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.writeStatus(ctx, &pool, cells, plan, scale, rollout, demand, demandKnown,
		freeGPUs, health, cap0, capacityKnown, reachable, reachReason, reachMsg)
	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	requeue := requeueSteady
	if _, _, creating, draining, _ := CountCells(cells); creating > 0 || draining > 0 || len(plan.Create) > 0 {
		requeue = requeueProgressing
	}
	if plan.RequeueAfter > 0 && plan.RequeueAfter < requeue {
		requeue = plan.RequeueAfter
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// discoverCells rebuilds per-cell state from live objects in both clusters.
func (r *GPUCellPoolReconciler) discoverCells(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool, prov provisioner.Provisioner,
	inner kubernetes.Interface, provider capacity.Provider, reachable bool,
) ([]cellsv1alpha1.CellStatus, error) {
	previous := map[string]cellsv1alpha1.CellStatus{}
	for _, c := range pool.Status.Cells {
		previous[c.Name] = c
	}

	guests, err := prov.List(ctx, pool.Namespace, pool.Name)
	if err != nil {
		return nil, err
	}
	sort.Strings(guests) // stable status ordering regardless of list order

	out := make([]cellsv1alpha1.CellStatus, 0, len(guests))
	for _, name := range guests {
		idx, err := cellid.ParseIndex(pool.Name, name)
		if err != nil {
			continue // not one of ours despite the label
		}
		prev := previous[name]

		// Keep the rendered cloud-init current (node labels can change) before
		// re-observing; Ensure recreates the seed profile if it went missing.
		if err := r.ensureBootstrapSecret(ctx, pool, idx); err != nil {
			return nil, err
		}

		outer, err := prov.Ensure(ctx, r.cellRequest(pool, idx))
		if err != nil {
			return nil, fmt.Errorf("observing cell %s: %w", name, err)
		}

		// Cell names are REUSED: index 0 is always <pool>-0, so a replacement lands on
		// the name of the cell it replaced. A status row for a different incarnation
		// must not lend its phase to the new one — carrying `Draining` across made a
		// freshly created cell inherit its predecessor's teardown and be deleted on the
		// very pass that created it, forever, with no timeout that could ever break it.
		// The guest UID distinguishes them, exactly as it does for a stale workload
		// Node. The failure counter is the one thing that must survive, because the
		// replacement backoff is counted per index, not per incarnation.
		if prev.GuestUID != "" && outer.UID != "" && prev.GuestUID != outer.UID {
			prev = cellsv1alpha1.CellStatus{
				Name: prev.Name, Index: prev.Index, FailureCount: prev.FailureCount,
			}
		}

		obs := Observation{
			Current:           phaseOr(prev.Phase, cellsv1alpha1.CellPhasePending),
			LastTransition:    transitionOr(prev.LastTransitionTime, r.now()),
			Now:               r.now(),
			Outer:             outer,
			WorkloadReachable: reachable,
			ExpectedDevices:   expectedDevices(pool),
			BootstrapTimeout:  durationOr(pool.Spec.Bootstrap.ReadyTimeout, 15*time.Minute),
			CapacityTimeout:   durationOr(pool.Spec.Capacity.ReadyTimeout, 5*time.Minute),
		}

		if reachable {
			node, nErr := workload.GetNodeState(ctx, inner, name, obs.Now)
			if nErr != nil {
				return nil, fmt.Errorf("cell %s: %w", name, nErr)
			}
			obs.Node = node
			// A foreign identity label alone does NOT make a Node stale. The
			// replacement's own kubelet adopts the existing object and keeps the
			// label it finds there, so a live kubelet means "adopt and re-label",
			// and only a Node nobody is heartbeating for is a phantom to reap.
			// Getting this wrong deletes the live cell's Node, and a kubelet whose
			// Node is deleted under it never re-registers (#14).
			obs.StaleNode = node.Exists && !node.KubeletLive &&
				cellid.IsStaleNode(node.Labels, outer.UID)

			if node.Exists && !obs.StaleNode {
				// The kubelet may not have applied the identity labels itself, and
				// an adopted Node still carries the previous incarnation's; patching
				// them here is the reliable path, and is what claims an adoption.
				if _, mErr := workload.EnsureNodeMetadata(ctx, inner, name,
					cellid.NodeLabels(pool.Name, idx, outer.UID, nodeLabels(pool)),
					nodeAnnotations(pool), nodeTaints(pool)); mErr != nil {
					return nil, fmt.Errorf("cell %s node metadata: %w", name, mErr)
				}
			}
			if provider != nil && node.Exists && node.Ready {
				if h, hErr := provider.Health(ctx, []string{name}); hErr == nil {
					obs.ProviderReady = h.Ready
				}
				if d, dErr := provider.Devices(ctx, name); dErr == nil {
					obs.CapacityDevices = d
				}
			}
		}

		// A draining cell keeps its phase; teardown owns it.
		if prev.Phase == cellsv1alpha1.CellPhaseDraining || prev.Phase == cellsv1alpha1.CellPhaseDeleting {
			obs.Current = prev.Phase
		}

		dec := AdvanceCell(obs)
		if dec.ReapStaleNode && reachable {
			if err := workload.DeleteNode(ctx, inner, name); err != nil {
				return nil, fmt.Errorf("reaping stale node for %s: %w", name, err)
			}
			r.event(pool, corev1.EventTypeWarning, cellsv1alpha1.ReasonNodeNameCollision,
				fmt.Sprintf("removed a workload Node left by a previous incarnation of cell %s", name))
		}

		out = append(out, r.cellStatus(prev, pool.Name, pool.Namespace, name, idx, outer, obs, dec))
	}

	// Carry forward a row for a failed cell whose guest we have already deleted.
	// The row is what remembers the failure count and the backoff; without it a
	// replaced cell starts from zero and the pool retries a broken configuration
	// every thirty seconds forever. The tombstone claims no index, so the create
	// path refills exactly that slot.
	live := map[string]bool{}
	for _, c := range out {
		live[c.Name] = true
	}
	for name, prev := range previous {
		if live[name] {
			continue
		}
		// This row has no guest. Either it becomes a tombstone, or it is dropped —
		// and dropping it is the last moment anything remembers the cell existed.
		keepTombstone := prev.Phase == cellsv1alpha1.CellPhaseFailed && prev.Index < pool.Spec.Replicas
		if keepTombstone {
			prev.GuestUID = ""
			prev.NodeName = ""
			prev.Devices = nil
			prev.CapacityDevices = 0
			out = append(out, prev)
			continue
		}
		// Otherwise the row is dropped, and its workload Node is left behind. That is
		// what sets up the collision in #14 — but it is NOT cleaned up here: at the
		// moment a row is dropped the cell's kubelet has only just died, so its lease
		// still looks fresh and the Node is (correctly) not reapable yet. A one-shot
		// attempt here reaps nothing and then nothing remembers the cell. The
		// idempotent sweep in reapOrphanNodes owns this.
	}
	if reachable {
		keep := make(map[string]bool, len(out))
		for _, c := range out {
			keep[c.Name] = true
		}
		if err := r.reapOrphanNodes(ctx, inner, pool, keep); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// cellStatus folds a decision into the cell's status row, carrying the failure
// count and transition time forward.
// reapOrphanNodes deletes workload Nodes that carry this pool's label but no
// longer correspond to any of its cells, once nothing is heartbeating for them.
//
// It exists because a retired cell is forgotten: its status row is dropped, and at
// that instant its kubelet has only just stopped, so the Node is not yet safe to
// delete. Anything that tried to clean up at drop time would find a Node that still
// looks live, skip it, and never look again — leaving the Node for the next cell at
// that index to adopt, which is the collision in #14. So the sweep is keyed on the
// pool LABEL and runs every reconcile: it converges instead of getting one chance.
//
// keep is the set of cell names the pool currently accounts for, including cells
// mid-teardown — their Nodes belong to the drain path, not here.
func (r *GPUCellPoolReconciler) reapOrphanNodes(
	ctx context.Context, inner kubernetes.Interface,
	pool *cellsv1alpha1.GPUCellPool, keep map[string]bool,
) error {
	nodes, err := workload.ListPoolNodes(ctx, inner, pool.Name, r.now())
	if err != nil {
		// A pool whose nodes cannot be listed is not a pool whose nodes should be
		// deleted; the reachability condition already reports the transport.
		logf.FromContext(ctx).V(1).Info("orphan node sweep skipped", "error", err.Error())
		return nil
	}
	for name, node := range nodes {
		if keep[name] || node.KubeletLive {
			continue
		}
		if err := workload.DeleteNode(ctx, inner, name); err != nil {
			return fmt.Errorf("removing orphan node %s: %w", name, err)
		}
		r.event(pool, corev1.EventTypeNormal, cellsv1alpha1.ReasonNodeNameCollision,
			fmt.Sprintf("removed workload Node %s: it carries this pool's label, "+
				"belongs to no current cell, and no kubelet is heartbeating for it", name))
	}
	return nil
}

func (r *GPUCellPoolReconciler) cellStatus(
	prev cellsv1alpha1.CellStatus, pool, namespace, name string, idx int32,
	outer provisioner.OuterState, obs Observation, dec Decision,
) cellsv1alpha1.CellStatus {
	c := cellsv1alpha1.CellStatus{
		Name:               name,
		Index:              idx,
		Phase:              dec.Phase,
		Message:            dec.Message,
		GuestUID:           outer.UID,
		TemplateHash:       outer.TemplateHash,
		HostNode:           outer.HostNode,
		Devices:            outer.GPUDevices,
		NodeReady:          obs.Node.Ready,
		CapacityDevices:    int32(obs.CapacityDevices),
		FailureCount:       prev.FailureCount,
		LastTransitionTime: prev.LastTransitionTime,
	}
	if obs.Node.Exists {
		c.NodeName = name
	}
	if prev.Phase != dec.Phase || c.LastTransitionTime == nil {
		nowT := metav1.NewTime(r.now())
		c.LastTransitionTime = &nowT
		if dec.Phase == cellsv1alpha1.CellPhaseFailed && prev.Phase != cellsv1alpha1.CellPhaseFailed {
			c.FailureCount = prev.FailureCount + 1
		}
		if prev.Phase != "" {
			poolmetrics.CellTransitionsTotal.WithLabelValues(
				pool, namespace, string(prev.Phase), string(dec.Phase)).Inc()
		}
		// Startup is measured from the guest's creation, not from the first phase
		// we happened to observe, and only on the FIRST time a cell reaches Ready
		// (a later Ready after a regression is not a startup).
		if dec.Phase == cellsv1alpha1.CellPhaseReady && prev.Phase != cellsv1alpha1.CellPhaseReady &&
			outer.CreatedAt != nil && !outer.CreatedAt.IsZero() && !prev.ReadyOnce {
			poolmetrics.CellStartupSeconds.WithLabelValues(pool, namespace).
				Observe(r.now().Sub(outer.CreatedAt.Time).Seconds())
		}
	}
	if dec.Phase == cellsv1alpha1.CellPhaseReady {
		c.ReadyOnce = true
	}
	return c
}

func (r *GPUCellPoolReconciler) createCell(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool, prov provisioner.Provisioner, idx int32,
) error {
	if err := cellid.ValidatePool(pool.Name); err != nil {
		return err
	}
	if err := r.ensureBootstrapSecret(ctx, pool, idx); err != nil {
		return err
	}
	_, err := prov.Ensure(ctx, r.cellRequest(pool, idx))
	return err
}

// ensureBootstrapSecret renders this cell's cloud-init from the user's template
// and stores it in a per-cell Secret the seed profile references.
//
// It must exist BEFORE the guest, or the VM boots with no cloud-init and never
// joins. It is rewritten only when the content actually differs, because the
// render is deterministic and rewriting a Secret every reconcile is churn.
func (r *GPUCellPoolReconciler) ensureBootstrapSecret(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool, idx int32,
) error {
	// A Cluster API pool with its own bootstrap template gets its join data from the
	// workload cluster's bootstrap provider, so there is nothing to render and no
	// join Secret to demand.
	if capiOwnsBootstrap(pool) {
		return nil
	}

	ref := pool.Spec.Bootstrap.JoinSecretRef
	if ref == nil {
		return fmt.Errorf("spec.bootstrap.joinSecretRef is required for the %s provider",
			cellsv1alpha1.BootstrapProviderOpaque)
	}

	srcKey := pool.Spec.Bootstrap.JoinSecretKey
	if srcKey == "" {
		srcKey = bootstrap.SecretKey
	}
	var src corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: ref.Name}, &src); err != nil {
		return fmt.Errorf("reading join secret %s/%s: %w", pool.Namespace, ref.Name, err)
	}
	tmpl, ok := src.Data[srcKey]
	if !ok {
		return fmt.Errorf("join secret %s/%s has no %q key", pool.Namespace, ref.Name, srcKey)
	}

	name := cellid.Name(pool.Name, idx)
	rendered, err := bootstrap.Render(tmpl, bootstrap.Values{
		CellName:        name,
		PoolName:        pool.Name,
		NodeLabels:      bootstrap.NodeLabelArg(cellid.NodeLabels(pool.Name, idx, "", nodeLabels(pool))),
		NodeIPInterface: pool.Spec.Cell.NodeIPFrom,
		ExpectedGPUs:    expectedDevices(pool),
	})
	if err != nil {
		return fmt.Errorf("rendering cloud-init for cell %s: %w", name, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pool.Namespace,
			Name:      cellid.BootstrapSecretName(name),
			Labels:    cellid.GuestLabels(pool.Name, idx),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				pool, cellsv1alpha1.GroupVersion.WithKind("GPUCellPool"))},
		},
		// Two keys, same bytes. KubeSwift's seed reads "user-data"; Cluster API's
		// bootstrap contract reads "value" from a dataSecretName Secret. Writing
		// both means one rendered Secret serves either provisioner instead of the
		// ClusterAPI path silently producing a Machine that never gets its data.
		Data: map[string][]byte{
			bootstrap.SecretKey:     rendered,
			bootstrap.CAPISecretKey: rendered,
		},
	}

	var existing corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return fmt.Errorf("reading bootstrap secret %s: %w", desired.Name, err)
	}
	if string(existing.Data[bootstrap.SecretKey]) == string(rendered) {
		return nil
	}
	existing.Data = desired.Data
	return r.Update(ctx, &existing)
}

// progressDrain cordons a draining cell, waits for its GPU to be released, then
// removes it. poolDeleting relaxes the wait to a bounded one (see AdvanceDrain).
func (r *GPUCellPoolReconciler) progressDrain(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool, prov provisioner.Provisioner,
	provider capacity.Provider, inner kubernetes.Interface,
	cell *cellsv1alpha1.CellStatus, poolDeleting bool,
) error {
	var (
		allocs      capacity.Allocations
		allocsKnown bool
	)
	if inner != nil && provider != nil {
		// Cordon BEFORE counting: counting first races the scheduler placing one
		// more pod.
		if err := workload.Cordon(ctx, inner, cell.Name); err != nil {
			return err
		}
		if a, err := provider.Allocations(ctx, cell.Name); err == nil {
			allocs, allocsKnown = a, true
		}
	}

	dec := AdvanceDrain(allocs, allocsKnown, poolDeleting,
		deletionPolicy(pool) == cellsv1alpha1.DeletionPolicyForce,
		transitionOr(cell.LastTransitionTime, r.now()), r.now(), drainTimeout(pool))

	cell.Message = dec.Message
	if !dec.Proceed {
		return nil
	}
	return r.deleteCell(ctx, pool, prov, *cell, true)
}

// deleteCell removes a cell: workload Node first (so the kubelet cannot
// re-register a Node we have stopped tracking), then the drain finalizer, then
// the outer objects.
func (r *GPUCellPoolReconciler) deleteCell(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool, prov provisioner.Provisioner,
	cell cellsv1alpha1.CellStatus, reapNode bool,
) error {
	idx := cell.Index
	req := r.cellRequest(pool, idx)

	if reapNode {
		if inner, ok, _, _ := r.workloadClient(ctx, pool); ok {
			if err := workload.DeleteNode(ctx, inner, cell.Name); err != nil {
				return err
			}
		}
	}
	if sg, ok := prov.(provisioner.DrainFinalizerClearer); ok {
		if err := sg.ClearDrainFinalizer(ctx, req); err != nil {
			return err
		}
	}
	if _, err := prov.Delete(ctx, req); err != nil {
		return err
	}
	return nil
}

func (r *GPUCellPoolReconciler) reconcileDeletion(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool, prov provisioner.Provisioner,
) (ctrl.Result, error) {
	inner, reachable, _, _ := r.workloadClient(ctx, pool)
	var provider capacity.Provider
	if reachable {
		provider = r.provider(inner, hamiMode(pool))
	}

	// Through the provisioner, NOT a SwiftGuest list. A ClusterAPI pool owns
	// Machines, so listing guests found nothing, drained nothing, and let the pool
	// remove its own finalizer — orphaning every cell Machine with our drain
	// finalizer still on it (unclearable, and still holding a GPU claim). Measured on
	// a live CAPI cluster.
	guests, err := prov.List(ctx, pool.Namespace, pool.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(guests) > 0 {
		for _, name := range guests {
			idx, err := cellid.ParseIndex(pool.Name, name)
			if err != nil {
				continue
			}
			cell := cellsv1alpha1.CellStatus{Name: name, Index: idx, Phase: cellsv1alpha1.CellPhaseDraining}
			for _, c := range pool.Status.Cells {
				if c.Name == name {
					cell = c
				}
			}
			if err := r.progressDrain(ctx, pool, prov, provider, inner, &cell, true); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: requeueProgressing}, nil
	}

	if controllerutil.RemoveFinalizer(pool, cellsv1alpha1.FinalizerPool) {
		if err := r.Update(ctx, pool); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing pool finalizer: %w", err)
		}
	}
	r.Clients.Forget(clientKey(pool))
	poolmetrics.ForgetPool(pool.Namespace, pool.Name)
	return ctrl.Result{}, nil
}

func (r *GPUCellPoolReconciler) writeStatus(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool,
	cells []cellsv1alpha1.CellStatus, plan MembershipPlan,
	scale ScaleDecision, rollout RolloutDecision, demand capacity.Demand, demandKnown bool,
	freeGPUs *int, health capacity.Health, cap0 capacity.Capacity, capacityKnown bool,
	reachable bool, reachReason, reachMsg string,
) {
	total, ready, creating, draining, failed := CountCells(cells)

	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Replicas = total
	pool.Status.ReadyCells = ready
	pool.Status.CreatingCells = creating
	pool.Status.DrainingCells = draining
	pool.Status.FailedCells = failed
	pool.Status.Cells = cells
	pool.Status.DesiredReplicas = scale.Desired

	if autoscalingEnabled(pool) && demandKnown {
		observed := metav1.NewTime(r.now())
		pool.Status.Demand = &cellsv1alpha1.DemandStatus{
			PendingRequests:      int32(demand.PendingRequests),
			SatisfiableByOneCell: int32(demand.SatisfiableByOneCell),
			LastObserved:         &observed,
		}
	}

	held := 0
	for _, c := range cells {
		held += len(c.Devices)
	}
	phys := &cellsv1alpha1.PhysicalCapacityStatus{GPUs: int32(held)}
	if freeGPUs != nil {
		f := int32(*freeGPUs)
		phys.FreeGPUsInCluster = &f
	}
	if len(cap0.ByModel) > 0 {
		phys.Model = cap0.ByModel[0].Model
	}
	pool.Status.PhysicalCapacity = phys

	// Stale-but-honest beats fresh-but-wrong: an unreadable provider retains the
	// previous numbers and their timestamp rather than reporting zero.
	if capacityKnown {
		pool.Status.WorkloadCapacity = WorkloadCapacityStatus(
			providerName(pool), hamiMode(pool), cap0, metav1.NewTime(r.now()))
	}

	waiting := 0
	for _, c := range cells {
		if c.Phase == cellsv1alpha1.CellPhaseAllocatingGPU || c.Phase == cellsv1alpha1.CellPhasePending {
			waiting++
		}
	}

	r.publishMetrics(pool, cells, scale, freeGPUs, cap0, capacityKnown)

	pool.Status.Conditions = ApplyConditions(pool.Status.Conditions, ComputeConditions(ConditionInput{
		Generation: pool.Generation,
		// The scaling decision, NOT spec.replicas: with autoscaling on, spec.replicas
		// is not the target. Comparing against it made Ready wrong in both directions
		// — a pool correctly holding 2 autoscaled cells read "2 of 1 cells are Ready"
		// and False, and one correctly holding none read "0 of 1".
		Desired:            scale.Desired,
		Ready:              ready,
		WorkloadReachable:  reachable,
		WorkloadReason:     reachReason,
		WorkloadMessage:    reachMsg,
		ProviderHealth:     health,
		CapacityKnown:      capacityKnown,
		Capacity:           cap0,
		FreeGPUs:           freeGPUs,
		CellsWaitingForGPU: waiting,
		Membership:         plan,
		Rollout:            rollout,
		Progressing:        creating > 0 || draining > 0 || len(plan.Create) > 0,
		Autoscaling:        autoscalingEnabled(pool),
		Scale:              scale,
		DemandKnown:        demandKnown,
	}))
	_ = ctx
}

// workloadClient resolves the pool's workload-cluster client. It returns
// reachable=false with a reason rather than an error, because an unreachable
// workload cluster is a state to report, not a reconcile failure to retry blindly.
func (r *GPUCellPoolReconciler) workloadClient(
	ctx context.Context, pool *cellsv1alpha1.GPUCellPool,
) (kubernetes.Interface, bool, string, string) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Spec.WorkloadCluster.KubeconfigSecretRef.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		reason := cellsv1alpha1.ReasonCredentialInvalid
		if !apierrors.IsNotFound(err) {
			reason = cellsv1alpha1.ReasonUnreachable
		}
		return nil, false, reason, fmt.Sprintf("reading kubeconfig secret %s: %v", key, err)
	}

	cs, err := r.Clients.For(clientKey(pool), &secret, pool.Spec.WorkloadCluster.Key)
	if err != nil {
		return nil, false, cellsv1alpha1.ReasonCredentialInvalid, err.Error()
	}
	if err := workload.Probe(ctx, cs); err != nil {
		return nil, false, workload.ClassifyError(err), err.Error()
	}
	return cs, true, cellsv1alpha1.ReasonConnected, ""
}

// cellRequest builds the provisioner request for one cell. The seed profile points
// at the PER-CELL rendered Secret (see ensureBootstrapSecret), never at the user's
// template directly, so the join credential stays in Secrets and each cell gets
// its own substituted cloud-init.
func (r *GPUCellPoolReconciler) cellRequest(pool *cellsv1alpha1.GPUCellPool, idx int32) provisioner.CellRequest {
	name := cellid.Name(pool.Name, idx)
	req := provisioner.CellRequest{
		Pool:                pool.Name,
		Namespace:           pool.Namespace,
		Index:               idx,
		CellName:            name,
		GuestTemplate:       pool.Spec.Cell.GuestTemplate.Raw,
		BootstrapSecretName: cellid.BootstrapSecretName(name),
		BootstrapSecretKey:  bootstrap.SecretKey,
		Hostname:            name,
		NodeIPFrom:          pool.Spec.Cell.NodeIPFrom,
		SpreadPolicy:        pool.Spec.Cell.SpreadPolicy,
		ClusterAPI:          pool.Spec.Cell.ClusterAPI,
		TemplateHash:        cellid.TemplateHash(pool.Spec.Cell.GuestTemplate.Raw),
		OwnerRefs: []metav1.OwnerReference{*metav1.NewControllerRef(
			pool, cellsv1alpha1.GroupVersion.WithKind("GPUCellPool"))},
	}
	if pool.Spec.Bootstrap.Hostname == "None" {
		req.Hostname = ""
	}

	gpu := pool.Spec.Cell.GPU
	req.GPU = provisioner.GPUBinding{Backend: gpu.Backend}
	if req.GPU.Backend == "" {
		req.GPU.Backend = cellsv1alpha1.GPUBackendDRA
	}
	if gpu.DRA != nil {
		req.GPU.ResourceClaimTemplateName = gpu.DRA.ResourceClaimTemplateName
		req.GPU.ResourceClaimName = gpu.DRA.ResourceClaimName
		req.GPU.RequestName = gpu.DRA.RequestName
		req.GPU.Tier = gpu.DRA.Tier
		req.GPU.Hugepages = gpu.DRA.Hugepages
	}
	if gpu.Native != nil {
		req.GPU.GPUProfileName = gpu.Native.GPUProfileRef.Name
	}
	return req
}

// SetupWithManager wires the reconciler and its watches.
func (r *GPUCellPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clients == nil {
		r.Clients = workload.NewClientCache()
	}
	if r.Clock == nil {
		r.Clock = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cellsv1alpha1.GPUCellPool{}).
		Named("gpucellpool").
		Complete(r)
}

// provisioner picks the cell provisioner named by the pool.
//
// An unknown value returns an error rather than quietly falling back to
// SwiftGuest: the field decides what objects get created in the infrastructure
// cluster, and silently ignoring it would give an operator a pool that looks
// configured for Cluster API while creating bare SwiftGuests.
func (r *GPUCellPoolReconciler) provisioner(pool *cellsv1alpha1.GPUCellPool) (provisioner.Provisioner, error) {
	if r.NewProvisioner != nil {
		return r.NewProvisioner(r.Client), nil
	}
	switch name := pool.Spec.Cell.Provisioner; name {
	case "", provisioner.ProvisionerSwiftGuest:
		return provisioner.NewSwiftGuestProvisioner(r.Client), nil
	case provisioner.ProvisionerClusterAPI:
		return provisioner.NewClusterAPIProvisioner(r.Client), nil
	default:
		return nil, fmt.Errorf("unknown cell provisioner %q", name)
	}
}

func (r *GPUCellPoolReconciler) provider(cs kubernetes.Interface, mode string) capacity.Provider {
	if r.NewProvider != nil {
		return r.NewProvider(cs, mode)
	}
	return capacity.NewHAMiProvider(cs, mode)
}

func (r *GPUCellPoolReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *GPUCellPoolReconciler) event(pool *cellsv1alpha1.GPUCellPool, kind, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	// The current events API wants an action as well as a reason. Our reasons are
	// already action-shaped (CellCreating, ScaledUp, DrainTimedOut), so they serve
	// as both rather than inventing a second vocabulary.
	r.Recorder.Eventf(pool, nil, kind, reason, reason, "%s", msg)
}

func (r *GPUCellPoolReconciler) markDraining(cells []cellsv1alpha1.CellStatus, name string) {
	for i := range cells {
		if cells[i].Name == name {
			cells[i].Phase = cellsv1alpha1.CellPhaseDraining
			nowT := metav1.NewTime(r.now())
			cells[i].LastTransitionTime = &nowT
		}
	}
}
