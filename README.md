# HyperFleet Applier

Go library for the HyperFleet **desire contract** and store backends. A desire is
the declarative intent for a single Kubernetes resource on a management cluster
(apply, delete, or read). Store backends persist that intent and its status so
appliers can reconcile resources without ManifestWork-style bulk transport.

It collaborates with other HyperFleet components:

* **[API](https://github.com/openshift-hyperfleet/hyperfleet-api)** is the source of truth for fleet resource desired state and aggregated status
* **[Sentinel](https://github.com/openshift-hyperfleet/hyperfleet-sentinel)** polls the API and publishes CloudEvents that drive reconciliation
* **[Adapter](https://github.com/openshift-hyperfleet/hyperfleet-adapter)** listens for those events, applies changes, and reports status back to the API

## Quick Start

### Try Locally

**Prerequisites:** Go 1.26+, Make

```bash
make build
make test
make lint
```

Run `make help` for the full list of targets.

### Run the applier

The binary follows the same command and configuration conventions as the other HyperFleet services:

```bash
make run
```

Override the default configuration path with `make run CONFIG=/path/to/applier.yaml`. The shared
Kubernetes discovery cache is invalidated using `discovery_refresh_interval`, allowing newly
installed CRDs to be reconciled without restarting the applier. All three controllers use
`poll_interval`.

`make run` uses `$KUBECONFIG` when set, otherwise `$HOME/.kube/config`. Override it with
`make run KUBE_CONFIG_PATH=/path/to/kubeconfig`. Redis must already be reachable at the URL in the
configuration file (or supplied through `HYPERFLEET_REDIS_URL`).

Configuration precedence follows the HyperFleet convention: command-line flags, then
`HYPERFLEET_*` environment variables, then YAML, then defaults. Use
`bin/hyperfleet-applier config-dump --config configs/applier.yaml` to inspect the merged,
redacted configuration.

### Package layout

| Package | Description |
|---------|-------------|
| [`pkg/desire`](pkg/desire) | Desire types (`ApplyDesire`, `DeleteDesire`, `ReadDesire`), identity validation, `SpecStore` / `StatusStore` interfaces |
| [`pkg/desire/store/memory`](pkg/desire/store/memory) | In-memory store for unit tests |
| [`pkg/desire/store/redis`](pkg/desire/store/redis) | Redis-backed store with WATCH/MULTI/EXEC CAS |
| [`pkg/desire/store/conformance`](pkg/desire/store/conformance) | Shared conformance suite exercised by both backends |

```bash
go get github.com/openshift-hyperfleet/hyperfleet-applier
```

## Documentation

### For Developers

| Resource | Description |
|----------|-------------|
| `make help` | Build, test, lint, format, and verify targets |
| [`pkg/desire` package docs](pkg/desire/doc.go) | Desire model, identity rules, and store contracts |

### Architecture

| Resource | Description |
|----------|-------------|
| [HyperFleet Architecture](https://github.com/openshift-hyperfleet/architecture) | System design |
| [HyperFleet API Spec](https://github.com/openshift-hyperfleet/hyperfleet-api-spec) | API contract |
| [Broker Library](https://github.com/openshift-hyperfleet/hyperfleet-broker) | Messaging abstraction |
| [Infrastructure](https://github.com/openshift-hyperfleet/hyperfleet-infra) | Deployment automation |

## Security posture

Credentials are stored scoped to the applier's own partition by convention.
Cryptographic enforcement of that scoping arrives with the production backends.

### FIPS Compliance:

As part of https://redhat.atlassian.net/browse/HYPERFLEET-1601 - decided to defer FIPS compliant images until compliance requirement comes in.
If want to build a FIPS compliant applier image -- add these Go variables to the `Dockerfile` and Makefile
```bash
CGO_ENABLED=1 and GOEXPERIMENT=strictfipsruntime go ...
```

Reference on this: https://developers.redhat.com/articles/2025/01/23/fips-mode-red-hat-go-toolset

## Contributing

1. Verify you're a member of the `openshift-hyperfleet` organization
2. Confirm you're added to the hyperfleet team
3. Code reviews and approvals are managed through the OWNERS file

For access issues, contact a repository administrator or organization owner.

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.
