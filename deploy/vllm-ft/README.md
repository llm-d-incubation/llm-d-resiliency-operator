# Enable vLLM fault handling

This example targets one `wide-ep` LWS in `default`, with two Pods and two original
DP ranks per Pod. Adapt the namespace and workload name in `operator.yaml` and
`access.yaml`. IRO reads group size from the LWS spec and the original rank layout
from each Pod's runtime observer. Inference probes omit the model name so vLLM
uses its served base model.

1. Build the operator image and set its reference in the overlay.
2. Install the [runtime bridge](../../runtime/vllm/README.md) in the vLLM image.
   Configure supervisor probes and native per-rank EPP health filtering as described there.
3. Review `kubectl kustomize deploy/vllm-ft`, then apply the overlay.

The operator requires LWS, its RecoveryRequest CRD, and a vLLM runtime with the
FT capabilities listed in the [runtime bridge](../../runtime/vllm/README.md).
The overlay includes the RecoveryRequest CRD and existing operator resources.
Engine fault handling is disabled without
`--enable-vllm-ft`; enabling it also requires `--leader-elect`.

Only the elected operator coordinates recovery. Its normal manager health probes
remain unchanged. Recovery progress and verified ranks are stored in
`wide-ep-ft-state`; they do not authorize requests. Keep this journal across
operator restarts. EPP independently routes from native engine health.

FT is scoped to one ordinal LWS group with `RecreateGroupOnPodRestart` and TP=1.
Install the observer before enabling FT: first discovery requires a valid,
Pod-UID-matched original rank layout from every Pod. IRO reuses a validated layout
while Pod/container identities and addresses remain unchanged, even if the observer
later becomes unavailable. Replacement groups require fresh discovery. The vLLM
runtime must support completion requests with the model omitted, selecting its
base model for verification.

vLLM validates survivor expert capacity during recovery; a rejected or failed
recovery requests group recreation. Validate the model/backend's FT behavior and
expert redundancy before deployment. This example does not establish that capacity.

Deploy this experimental feature with a fresh native-recovery journal. An older
admission-based prototype must recreate its reduced group before removing its
routing gate; its persisted exclusions are not supported by this implementation.
