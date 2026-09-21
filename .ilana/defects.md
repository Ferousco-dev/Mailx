# Defect Log

Found during the 2026-09-21 historical audit. Reported only; NOT fixed (audit scope forbids production changes). Each needs its own change request.

- DEF-001 [medium, open, design gap]: Verified domain ownership is not consulted by `POST /v1/emails` (`internal/api/email_handler.go`); any tenant can submit any From domain. See RSK-006.
- DEF-002 [medium, open, incomplete feature]: DSNs are built and serialized in `worker.handleTerminalFailure` then discarded; senders receive no bounce email. See RSK-005.
- DEF-003 [low, open, test scaffold hazard]: `newMux` in `internal/api/routes.go` defaults to a zero-byte-key `SecretBox` and an unrestricted `URLPolicy` when no `routeServices` are passed. Production is unaffected (`api.Config.Webhooks` is required and validated) but tests or future callers using the default would encrypt secrets with an all-zero key.
- DEF-004 [medium, FIXED in v0.23, found during Compose e2e]: `worker.runWorker` returned on ANY `Queue.Claim` error, so a Redis outage made `Pool.Run` exit and `runComponents` stopped the whole process (liveness lost, no recovery). Fix: log, count, back off 1 s and retry unless the context is done or the queue is closed (`internal/worker/worker.go`; test `TestWorkerSurvivesTransientQueueClaimFailureAndRecovers`). Durable semantics unchanged. The component-exit-stops-process design (`5ebd574`) is otherwise retained.
