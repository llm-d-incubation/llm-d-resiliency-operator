# Enable vLLM fault handling

This example targets one `wide-ep` LWS in `default`, with two Pods and two original
DP ranks per Pod. Adapt the namespace, workload name, model and validated minimum
survivor count in `operator.yaml` and `access.yaml`.

1. Build the operator image and set its reference in the overlay.
2. Install the [runtime bridge](../../runtime/vllm/README.md) in the vLLM image.
   Configure supervisor probes and native per-rank EPP health filtering as described there.
3. Review `kubectl kustomize deploy/vllm-ft`, then apply the overlay.

The operator requires LWS, its RecoveryRequest CRD, and a vLLM runtime with the
FT capabilities listed in the design. The overlay includes the RecoveryRequest
CRD and existing operator resources. Engine fault handling is disabled without
`--enable-vllm-ft`; enabling it also requires `--leader-elect`.

Only the elected operator coordinates recovery. Its normal manager health probes
remain unchanged. Recovery progress and verified ranks are stored in
`wide-ep-ft-state`; they do not authorize requests. Keep this journal across
operator restarts. EPP independently routes from native engine health.

FT is scoped to one ordinal LWS group with `RecreateGroupOnPodRestart` and TP=1.
The minimum survivor count must come from the deployment's expert-capacity
validation. This example does not establish that capacity for a particular model.

Deploy this experimental feature with a fresh native-recovery journal. An older
admission-based prototype must recreate its reduced group before removing its
routing gate; its persisted exclusions are not supported by this implementation.
