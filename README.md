# llm-d-resiliency-operator

[![CI](https://github.com/llm-d-incubation/llm-d-resiliency-operator/actions/workflows/ci-pr-checks.yaml/badge.svg)](https://github.com/llm-d-incubation/llm-d-resiliency-operator/actions/workflows/ci-pr-checks.yaml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

The llm-d Inference Resilience Operator (IRO) automatically coordinates hardware
fault events with the inference engine, sequencing the right engine-side response
and infrastructure-side recovery action to minimize serving interruption and
restore full capacity without manual intervention.

## Overview

Please refer to the [WIP design doc](https://docs.google.com/document/d/1q4V2CcWMSrufy5LJE_Fv49kwCoFoPSTrBrrayzNxRqY/edit?usp=sharing) and [proposal](https://github.com/llm-d/llm-d/blob/main/docs/proposals/inference-resilience-operator.md)

## Prerequisites

- Go 1.25+
- Docker (for container builds)
- [pre-commit](https://pre-commit.com/) (for local development)

## Quick Start

```bash
# Clone the repo
git clone https://github.com/llm-d-incubation/llm-d-resiliency-operator.git
cd llm-d-resiliency-operator

# Install pre-commit hooks
pre-commit install

# Build
make build

# Run tests
make test

# Run linters
make lint
```

Running locally with current shell's kubeconfig:
```bash
go run cmd/main.go --pod-namespace=llm-d-wide-ep --pod-label-key=llm-d.ai/inference-serving
```

This requires that an llm-d inference engine is running in the specified namespace and has the specified label.
It also requires the RecoveryRequest CRD to be installed:
```bash
kubectl apply -f deploy/kubernetes/crd.yaml
```

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidelines, coding standards, and how to submit changes.

### Common Commands

```bash
make help           # Show all available targets
make build          # Build the project
make test           # Run tests with race detection
make lint           # Run Go and Python linters
make fmt            # Format Go and Python code
make image-build    # Build multi-arch container image
make pre-commit     # Run pre-commit hooks
```

## Architecture

Please refer to the [WIP design doc](https://docs.google.com/document/d/1q4V2CcWMSrufy5LJE_Fv49kwCoFoPSTrBrrayzNxRqY/edit?usp=sharing).

### vLLM EngineAdapter

The opt-in adapter handles engine faults while LWS Pods remain running. It retries
usable ranks or excludes failed ranks, verifies inference, and resets the group
when in-place FT cannot complete. RecoveryRequest behavior is unchanged.

See the [deployment example](deploy/vllm-ft/README.md) and
[runtime bridge](runtime/vllm/README.md).

## Configuration

<!-- TODO: Document configuration options, environment variables, CLI flags -->

## Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

All commits must be signed off (DCO). See [PR_SIGNOFF.md](PR_SIGNOFF.md) for instructions.

## Security

To report a security vulnerability, please see [SECURITY.md](SECURITY.md).

## License

This project is licensed under the Apache License 2.0 - see [LICENSE](LICENSE) for details.
