# TODO

## Sync behavior

- Improve the pipeline for resolving `blocked_nonempty` sync roots.

  Current meaning: Syna refused to materialize the remote tracked root because
  the target path already exists and is not empty. This is a safety block to
  avoid overwriting local files during bootstrap.

  Example blocked path:

  ```text
  /root/test
  ```

  Current recovery steps:

  1. Inspect the target path:

     ```sh
     sudo ls -la /root/test
     ```

  2. Choose one recovery option.

     If `/root/test` is disposable:

     ```sh
     sudo rm -rf /root/test
     ```

     If the contents should be kept:

     ```sh
     sudo mv /root/test /root/test.backup
     sudo mkdir -p /root/test
     ```

  3. Wait up to 30 seconds, or restart the daemon. The client retries blocked
     roots automatically.

  4. Verify the root becomes active:

     ```sh
     syna status
     ```

  Note: if the target path is under `/root`, but the user expected to sync into
  their normal home directory, the daemon or CLI was likely run as root or with
  `sudo`. Stop running Syna as root, then reconnect or add the folder as the
  regular user so tracked paths resolve under the intended home directory.

- Re-test what happens when adding a non-existing directory.
  - Confirm whether Syna creates the directory immediately.
  - Confirm whether that creation leads to a confusing or broken state.
  - Decide whether the UX needs clearer output or safer behavior.

## Release pipeline

- Improve the release pipeline for server Docker deployments.
  - Improve `scripts/release.sh` so the release flow validates or builds the
    server Docker image locally.
  - Document a Coolify deployment path that uses Docker image pulling.
  - Document a manual deployment path with a concrete `docker run ...` command.
  - Make the release output clearly tell users which deployment path to choose.
