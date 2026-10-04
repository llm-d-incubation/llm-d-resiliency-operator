#!/usr/bin/env bash
# Copyright 2026 The llm-d Authors.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

: "${POD_UID:?}"
: "${IRO_RANK_START:?}" "${IRO_LOCAL_RANKS:?}" "${IRO_DP_SIZE:?}" "${IRO_DP_ADDRESS:?}"
if [[ $# -eq 0 ]]; then
  echo "Usage: launch.sh MODEL [vLLM options]" >&2
  exit 2
fi

RUNTIME_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export PYTHONPATH="${RUNTIME_DIR}${PYTHONPATH:+:${PYTHONPATH}}"

python3 "${RUNTIME_DIR}/rank_observer.py" \
  --start-rank "$IRO_RANK_START" --local-ranks "$IRO_LOCAL_RANKS" \
  --port "${IRO_OBSERVER_PORT:-9257}" &

exec python3 -m vllm.entrypoints.cli.main serve "$@" \
  --data-parallel-size "$IRO_DP_SIZE" \
  --data-parallel-size-local "$IRO_LOCAL_RANKS" \
  --data-parallel-start-rank "$IRO_RANK_START" \
  --data-parallel-address "$IRO_DP_ADDRESS" \
  --data-parallel-multi-port-external-lb \
  --data-parallel-supervisor-port "${IRO_SUPERVISOR_PORT:-9256}" \
  --tensor-parallel-size 1 --enable-expert-parallel --enable-fault-tolerance
