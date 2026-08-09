# Upgrading

## The one thing that is not automatic: the CRD

`helm upgrade` does **not** update a chart's `crds/`. Helm installs those files
once and never touches them again. Nothing warns you, and the upgrade succeeds.

What happens next is the problem. The apiserver validates against the schema it is
serving — the old one — and **silently drops** every field that schema does not
know. Not rejects: drops. You apply a pool spec, the apiserver stores a version of
it with your new fields removed, and reports success.

So after every `helm upgrade`, apply the CRD:

```bash
kubectl apply -f https://raw.githubusercontent.com/kubeswift-io/gpucellpool/vX.Y.Z/config/crd/bases/cells.kubeswift.io_gpucellpools.yaml
```

Order does not matter much — before or after the upgrade is fine — but it must
happen. `helm upgrade --install` does not do it, `--force` does not do it, and
reinstalling the release does not do it either (the CRD already exists).

### You do not have to remember

The manager compares the schema the cluster is serving with the one it was built
against, at startup, and names the exact fields being dropped:

```
ERROR  the cluster is serving an OLDER CRD than this operator was built against;
       the apiserver will SILENTLY DROP the fields below, so features that depend
       on them will appear to be accepted and then do nothing. helm upgrade does
       not update CRDs — apply it yourself
       {"crd": "gpucellpools.cells.kubeswift.io",
        "missingFields": ["v1alpha1.spec.updatePolicy"],
        "fix": "kubectl apply -f https://.../cells.kubeswift.io_gpucellpools.yaml"}
```

Check it after any upgrade:

```bash
kubectl -n gpucellpool-system logs deploy/gpucellpool | grep -i 'CRD schema'
```

`CRD schema matches this build` means there is nothing to do. If the operator's
credential cannot read CRDs the line says `CRD schema not compared` — the check is
read-only and optional (`apiextensions.k8s.io/customresourcedefinitions: get`), and
its absence degrades the check rather than the operator.

### What v0.1.0 → v0.1.1 drops if you skip it

Both fields `updatePolicy` added:

| Field | Consequence of the old schema |
|---|---|
| `spec.updatePolicy` | `type: RollingUpdate` is accepted and discarded. The pool reports `Manual` and never replaces a stale cell — the feature looks enabled and does nothing |
| `status.cells[].templateHash` | the operator's writes vanish, so `Updated` compares against an empty hash. An empty hash counts as current by design, so **drift reports as up-to-date forever** |

## The rest of an upgrade

```bash
helm upgrade gpucellpool oci://ghcr.io/kubeswift-io/charts/gpucellpool \
  --version X.Y.Z -n gpucellpool-system \
  -f <(helm get values gpucellpool -n gpucellpool-system -o yaml)
```

Two notes on that command:

- Pass the old values through a file rather than using `--reuse-values`, which
  skips defaulting for value blocks a new chart version added and can leave nil
  where the templates expect a map.
- `helm get values -o yaml` output is the values document itself, with no header
  line to strip.

Running pools are not disturbed by an operator upgrade: cells are separate objects
in the infrastructure cluster, and the manager rebuilds its view from their live
state on the first reconcile. There is no state in the manager to migrate.

The webhook's self-signed certificate survives upgrades — the chart reuses the
existing Secret rather than minting a new one, so the `caBundle` and the serving
cert cannot drift apart.

## Downgrading

Downgrade the release, and leave the CRD alone. A newer CRD serving an older
operator is safe: the extra fields are simply unused. Removing fields from a CRD
that stored objects still carry is not.
