# Syna Documentation Index

This directory is the implementation spec for Syna v1.

Syna is a Linux-only, many-way file sync system with end-to-end encryption. It has two runtime services:

- `syna-server`: the backend service, deployed in Docker on a VPS
- `syna`: the client binary, which exposes the CLI and runs the local background daemon

There is no third shipped client-daemon executable. `syna daemon` is a long-lived mode of the `syna` binary.

Read the documents in this order:

1. [01-product-scope.md](./01-product-scope.md)
2. [02-architecture.md](./02-architecture.md)
3. [03-crypto-and-trust.md](./03-crypto-and-trust.md)
4. [04-data-model-and-storage.md](./04-data-model-and-storage.md)
5. [05-sync-protocol.md](./05-sync-protocol.md)
6. [06-client.md](./06-client.md)
7. [07-backend.md](./07-backend.md)
8. [08-deployment-and-operations.md](./08-deployment-and-operations.md)
9. [09-build-plan.md](./09-build-plan.md)

The documents are intentionally prescriptive. Builders should follow them unless a later design pass explicitly replaces them.
