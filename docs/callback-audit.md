# Interaction callback audit

Interaction state publication must finish before application code runs. Pausing keeps its
in-progress guard active through finalization, redaction, tool events and delivery. Calls
accepted during finalization belong to the closing segment; calls accepted during delivery
belong to the next segment. Each accepted call is emitted and counted once.

| Callback family | Go behavior |
| --- | --- |
| Error handlers | Notices and contained build failures detached in `unlock`, reported outside `mu` |
| Redactors | Prompt/response masking outside `mu` |
| Adapter finalizers | `beforeEnd` detached under `mu`, invoked outside it |
| Tool filters/properties | Jobs copied and fired flag set under `mu`, hooks outside it |
| Transport/delivery/flush | Outbox detached before `Enqueue`; flush outside `mu` |
| Classifier/evaluator | Turn copied, work scheduled after response publication |
| JSON serialization hooks | Tool properties normalized/sized outside `mu`; delivery serialization outside `mu` |
| Identity/trace/disconnect/error classification | Startup or outside `mu` |
| Credit/pricing | No callable hook; static price registry |

Regression matrices in the pause tests exercise Pause, End and RecordCall in each callable
family above that runs on an existing interaction, including separate call/tool cap notices.
Go uses TryLock and bounded completion channels to detect callbacks invoked while the
interaction lock is held. Background hooks verify the ended interaction stays ended,
rejects new calls, retains prior usage and emits one classification or evaluation.
