# Per-user queue validation — 16 October 2026

The queue preserves four active requests per account. By default it permits
up to eight FIFO waiters per account for at most 60 seconds, at most half of the
fixed worker pool globally, and at most 16 MiB of combined waiting payloads.
No successful response or usage is fabricated. Genuine queue overflow, waiting
deadline expiry, RPS limits and upstream errors remain explicit errors.

Verified on the VPS with its native `/usr/bin/g++-9` toolchain:

- Limiter unit tests passed: FIFO, account isolation, per-user/global bounds,
  queued payload bound, cancellation, timeout, slot release and rate limits.
- HTTP fixture burst: six requests (streaming and JSON) completed successfully
  with at most two active for the test account; the SSE terminator stayed intact.
- Cancellation/isolation fixture: a disconnected waiter never reached upstream;
  another account was admitted while the first account's queue was occupied.
- Timeout fixture: deadline expiry returned 429; no duplicate upstream request
  or leaked slot appeared.

Fixtures use local HTTP servers and dummy credentials; no AI inference was
performed by these tests. Build on the laptop lacked libcurl development headers,
so the production-compatible binary was built and tested natively on Ubuntu VPS.

Deployment is prepared in `deployments/guard-user-queue.sh`. The initial attempt
waited for an idle guard window, then deferred because four requests remained
active. Production was not changed during that attempt. Installation requires
an idle switchover window; binary/config backups and automatic health rollback
are included. Customer databases and usage records are not rewritten.
