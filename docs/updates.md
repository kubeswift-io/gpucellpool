# Changing the cell template

Editing `spec.cell` — a new `imageRef` after a driver bump, a different
`guestClassRef`, another interface — changes the shape of a cell. Existing cells
are already running the old shape, and there is no in-place update of a VM's
image: adopting a new template means replacing cells.

The pool always tells you the cells have drifted. Whether it replaces them is
yours to decide.

## Seeing drift

Every cell records the template it was created from, and the `Updated` condition
compares that with the pool's current one:

```bash
kubectl get cellpool <name> -n <ns> \
  -o jsonpath='{range .status.conditions[?(@.type=="Updated")]}{.status} {.reason} {.message}{"\n"}{end}'

# which cells specifically
kubectl get cellpool <name> -n <ns> \
  -o jsonpath='{range .status.cells[*]}{.name} {.templateHash}{"\n"}{end}'
```

| `Updated` | Reason | Meaning |
|---|---|---|
| True | `AllCellsCurrent` | every cell matches the current template |
| False | `TemplateChanged` | cells are out of date and `updatePolicy.type` is `Manual`, so nothing will happen to them |
| False | `RollingUpdate` | a cell is being replaced right now |
| False | `UpdateBlocked` | replacement is wanted but cannot proceed — the message says why |

A cell with an empty `templateHash` is treated as current, not stale. It predates
the hash being read back, and treating unknown as out-of-date would replace a
whole healthy pool the first time you switched the policy on.

## Manual (the default)

Nothing is replaced. You do it when it suits you, one cell at a time:

```bash
# in the WORKLOAD cluster: stop new work landing, move what is there
kubectl cordon <cell>
kubectl drain <cell> --ignore-daemonsets --delete-emptydir-data

# in the INFRASTRUCTURE cluster: remove the cell; the pool refills the index
kubectl delete swiftguest <cell> -n <ns>        # or: kubectl delete machine <cell> -n <ns>
```

Wait for the replacement to reach `Ready` before doing the next one. The pool
refills the freed index from the current template, so the cell comes back with
the same name and the new shape.

## RollingUpdate

```yaml
spec:
  updatePolicy:
    type: RollingUpdate
```

The pool replaces stale cells itself — deliberately slowly, and through the same
gate automatic scale-down uses:

- **one cell at a time**, never while another cell is being created or drained;
- **only cells the capacity provider reports as idle.** A cell whose allocations
  cannot be read is never replaced either — "empty" and "unknown" are different
  answers;
- **not while the pool is resizing**, so a rollout cannot race a scaling decision
  for the index it is about to free;
- **not while the workload cluster is unreachable**, because then "is this cell
  busy?" has no answer;
- lowest index first, so the order is predictable.

The drain gate re-checks allocations again immediately before the cell is
removed, so a workload that lands between the decision and the deletion is still
safe.

### It can stall, and that is the point

On a pool whose stale cells all hold workloads, a rolling update makes no
progress — indefinitely. `Updated` stays False with reason `UpdateBlocked` and a
message naming the cause. It will not evict anything to make room: this operator
waits for a GPU to be released rather than taking it away, which is why it holds
no `pods/eviction` right in the workload cluster at all.

If you need the cell back sooner, drain it yourself with `kubectl drain` and the
rollout proceeds on its next pass.

### Capacity during a rollout

Replacing a cell costs its capacity for the duration of a full cell startup
(measured 4m45s). There is no surge: a cell holds a *physical* GPU, so bringing up
a replacement alongside the old one would need a spare device. On a single-cell
pool a rolling update therefore means a gap in service — which is a reason to run
`replicas: 2` if the workload cannot tolerate one.
