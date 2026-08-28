# TODO

## Sync behavior

- [x] Fix live-session expiry: renew before the bearer token expires, reconnect the
  WebSocket on any authenticated HTTP 401, and verify queued local changes flush
  after reauthentication.
