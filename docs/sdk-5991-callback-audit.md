# SDK-5991 callback audit

Interaction state publication must finish before application code runs. Pausing keeps its
in-progress guard active through finalization, redaction, tool events and delivery. Calls
accepted during finalization belong to the closing segment; calls accepted during delivery
belong to the next segment. Each accepted call is emitted and counted once.

| Callback family | Node | Python | Go |
| --- | --- | --- | --- |
| Error handlers | Cap notices after call/tool publication | Notice detached under `_lock`, reported outside it | Notices and contained build failures detached in `unlock`, reported outside `mu` |
| Redactors | Prompt/response preparation outside state publication | Prompt/response masking outside `_lock` | Prompt/response masking outside `mu` |
| Adapter finalizers | `beforeEnd` hooks detached before invocation | Streams detached under `_lock`, finalized outside it | `beforeEnd` detached under `mu`, invoked outside it |
| Tool filters/properties | Fired flag set before hooks | Fired flag set under `_lock`, hooks outside it | Jobs copied and fired flag set under `mu`, hooks outside it |
| Transport/delivery/flush | Pending batch detached before `track`; flush callbacks outside interaction operations | Prompt detached before `_send`; `track` and flush outside `_lock` | Outbox detached before `Enqueue`; flush outside `mu` |
| Classifier/evaluator | Scheduled after response publication | Turn copied, work scheduled after response publication | Turn copied, work scheduled after response publication |
| JSON serialization hooks | Property sizing/serialization outside publication | Event encoding outside `_lock` | Tool properties normalized/sized outside `mu`; delivery serialization outside `mu` |
| Identity/trace/disconnect/error classification | Startup or outside publication | Startup/request state or outside `_lock` | Startup or outside `mu` |
| Credit/pricing | No callable hook; static price registry | No callable hook; static price registry | No callable hook; static price registry |

Regression matrices in the pause tests exercise Pause, End and RecordCall in each callable
family above that runs on an existing interaction, including separate call/tool cap notices.
Python uses a second thread to detect an RLock held by the callback's caller; Go uses TryLock
and bounded completion channels. Background hooks verify the ended interaction stays ended,
rejects new calls, retains prior usage and emits one classification or evaluation.

The fixed schemas and SPEC_VERSION are unchanged in this round.
