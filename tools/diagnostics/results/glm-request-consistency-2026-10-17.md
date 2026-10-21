# GLM request consistency — 2026-10-17

Deployed router SHA-256 `f657e5d7b6c2e89443fef1e99b296c7c83e574bb29c08f217c5f4a1db063bc7b`.
Backup: `/var/backups/mot-router/glm-consistency-20261017T070042Z`.

## Findings and changes

- The translator, endpoint selector, and header builder previously read the mutable gate provider separately. One request now binds a copied profile for the entire operation, including retries.
- Automatic recovery previously verified the default DeepSeek model even when GLM failed. Recovery and manual refresh now verify the selected model. A concurrent recovery for another model cannot count as success for GLM.
- Greedy tool reduction produced a globally stored `read`-only roster from a DeepSeek probe. Production discovery now retains the full accepted native roster, performs no version-floor search, and requires baseline plus final confirmation before publishing. Existing last-good settings remain available when a new probe is inconclusive.
- Without native routing, the API path could send an inference and then send another fallback inference while discarding the first stream. It now performs one inference and retains its relay attribution.
- Recovery no longer replays an identical profile after a rejection.

## Local evidence

`go test -short -race -p 2 -buildvcs=false -timeout 60s ./...` passed. Focused server/gate tests passed again after adding the captured OMP schema fixture.

Isolated OMP 18.2.9 localhost capture used a dummy credential and made no upstream inference. It sent GLM to `/v1/chat/completions`, with `max_completion_tokens`, system/user messages, and 11 tools: read, bash, edit, eval, glob, grep, task, hub, todo, web_search, write. Tests verify those tool parameter schemas and output limit survive translation.

Regression fixtures cover mixed profiles, duplicate inference, model-specific recovery, unchanged-profile rejection, quota/unrelated rejection, and preservation of a last-good profile on failed verification.

## VPS evidence and remaining limit

Deployment completed; router, native runtime, NodeBank, and guard are active. The native runtime and selected node were preserved. Credentials were unchanged.

The targeted GLM baseline returned HTTP 429 with `server_error`: `Upstream request failed: Endpoint is unavailable.` This does **not** establish a user quota exhaustion; the body reports endpoint availability. The probe is correctly marked inconclusive. Playground and OMP-shaped production inference were consequently skipped, with no requested node rotation or repeated inference.

The production 403 is **not yet proven resolved**. Continue acceptance with the bounded GLM checker when the upstream endpoint is available. Do not infer success for GLM from a successful DeepSeek request, and do not label HTTP 429 as profile verification.
