package kit

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// openai-go and anthropic-sdk-go retry inside the client, below the middleware, and number each
// attempt in this header. Node.js and Python wrap the SDK method and see one call, so a failed
// attempt is held until the SDK shows what it does with it: reading the body of a failed response
// means it ends the call (recorded), sending the attempt again means a retry (dropped).
const retryCountHeader = "X-Stainless-Retry-Count"

// attempt is a failed attempt whose outcome waits on what the SDK does with its response.
type attempt struct {
	spec      *Spec
	call      *Call
	errorType string
	key       attemptKey
	// closed: the SDK closed the response unread, so it means to retry.
	closed atomic.Bool
	mu     sync.Mutex
	// stop cancels the context watch of an ephemeral attempt.
	stop func() bool
	// grace bounds the wait for a retry even when the request context never ends.
	grace *time.Timer
	// done releases Flush, which waits for a held attempt outside an interaction.
	done func()
}

// release lets Flush stop waiting for the attempt.
func (a *attempt) release() {
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	if done != nil {
		done()
	}
}

// attemptKey matches a retry to its failed attempt: same middleware, same open interaction
// (nil for a call outside one), same request.
type attemptKey struct {
	spec         any
	invocation   reflect.Value
	requestOwner *http.Request
	interaction  *rudderstackai.Interaction
	request      [sha256.Size]byte
}

// retried holds failed attempts by invocation and interaction. At most hook.MaxMapEntries
// are held; past it an attempt is recorded at once.
var retried = struct {
	mu            sync.Mutex
	m             map[attemptKey]*attempt
	byInteraction map[*rudderstackai.Interaction]map[attemptKey]struct{}
}{m: map[attemptKey]*attempt{}, byInteraction: map[*rudderstackai.Interaction]map[attemptKey]struct{}{}}

// heldLocked holds a under k.
func heldLocked(k attemptKey, a *attempt) {
	retried.m[k] = a
	if k.interaction == nil {
		return
	}
	keys := retried.byInteraction[k.interaction]
	if keys == nil {
		keys = map[attemptKey]struct{}{}
		retried.byInteraction[k.interaction] = keys
	}
	keys[k] = struct{}{}
}

// releasedLocked forgets the attempt held under k.
func releasedLocked(k attemptKey) {
	delete(retried.m, k)
	if keys := retried.byInteraction[k.interaction]; keys != nil {
		delete(keys, k)
		if len(keys) == 0 {
			delete(retried.byInteraction, k.interaction)
		}
	}
}

// retryNumber is the attempt number the SDK sent; ok is false without the header.
func retryNumber(req *http.Request) (n int, ok bool) {
	v := req.Header.Get(retryCountHeader)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func keyFor(ctx context.Context, s *Spec, req *http.Request, raw []byte) attemptKey {
	scope := s.RetryScope
	if scope == nil {
		scope = s
	}
	k := attemptKey{spec: scope, request: sha256.Sum256(append([]byte(req.Method+" "+req.URL.String()+"\n"), raw...))}
	// Stainless clones the request while retaining its per-invocation body factory.
	// reflect.Value retains that factory's identity without inspecting closure internals.
	if !hook.Tracing.EmitSpans() {
		k.spec = s
	} else if req.GetBody != nil {
		k.invocation = reflect.ValueOf(req.GetBody)
	} else {
		k.requestOwner = req
	}
	if i := rudderstackai.FromContext(ctx); i != nil && !i.Ended() {
		k.interaction = i
	}
	return k
}

// resume takes only the failed attempt belonging to this retry
// invocation. Other invocations remain held until their own completion or interaction end.
func resume(k attemptKey, n int) *attempt {
	retried.mu.Lock()
	var prev *attempt
	if n > 0 {
		if a, ok := retried.m[k]; ok {
			prev = a
			releasedLocked(k)
		}
	}
	var stale []*attempt
	if !hook.Tracing.EmitSpans() && k.interaction != nil {
		for key := range retried.byInteraction[k.interaction] {
			stale = append(stale, retried.m[key])
			releasedLocked(key)
		}
	}
	retried.mu.Unlock()
	if prev != nil {
		prev.claimed()
	}
	for _, a := range stale {
		a.settle()
	}
	return prev
}

// claimed stops the watches of an attempt that is retried or replaced.
func (a *attempt) claimed() {
	a.stopWatches()
	a.release()
}

func (a *attempt) stopWatches() {
	a.mu.Lock()
	if a.stop != nil {
		a.stop()
	}
	if a.grace != nil {
		a.grace.Stop()
	}
	a.mu.Unlock()
}

// take removes a from the held attempts; false when a retry or another settle got it first.
func (a *attempt) take() bool {
	retried.mu.Lock()
	defer retried.mu.Unlock()
	if retried.m[a.key] != a {
		return false
	}
	releasedLocked(a.key)
	return true
}

// settle records a held attempt as the failed call it turned out to be.
func (a *attempt) settle() {
	defer a.release()
	a.stopWatches()
	t := a.errorType
	if err := a.call.HTTPRequest.Context().Err(); err != nil {
		t = ErrorTypeOfTransport(err) // the SDK returned the context error instead of retrying
	}
	a.spec.failed(a.call, t)
}

// hold keeps a until it is read, retried, or settled at interaction/context end.
// With spans enabled, the retry grace also bounds holds on contexts that never end.
func (a *attempt) hold() {
	retried.mu.Lock()
	old := retried.m[a.key]
	if old == nil && len(retried.m) >= hook.MaxMapEntries() {
		retried.mu.Unlock()
		hook.Report("limit_reached", "MaxMapEntries (held retries)", errHeldFull)
		a.settle()
		return
	}
	heldLocked(a.key, a)
	retried.mu.Unlock()
	if old != nil { // an identical request that was never retried
		old.claimed()
		old.settle()
	}
	a.mu.Lock()
	if a.call.Ephemeral {
		// Tracked from now, not from the context watch: Flush right after the SDK returns must
		// still wait for the attempt the watch is about to settle.
		a.done = hook.Track()
		a.stop = context.AfterFunc(a.call.HTTPRequest.Context(), a.contextDone)
	}
	if a.call.Span != nil {
		a.grace = time.AfterFunc(retryGrace, a.settleIfTaken)
	}
	a.mu.Unlock()
	if !a.call.Ephemeral {
		OnBeforeEnd(a.call.Interaction, a.settleIfTaken)
	}
}

// contextDone settles an ephemeral attempt whose context ended. Once the SDK closed the response
// to retry, the end may be its own per-attempt timeout released just before the retry, so the
// retry gets a moment to claim the attempt first.
func (a *attempt) contextDone() {
	if a.closed.Load() {
		a.mu.Lock()
		if a.grace == nil {
			a.grace = time.AfterFunc(retryGrace, a.settleIfTaken)
		}
		a.mu.Unlock()
		return
	}
	a.settleIfTaken()
}

const retryGrace = time.Second

var errHeldFull = errors.New("rudderstackai: too many failed attempts held for a retry; recorded at once")

func (a *attempt) settleIfTaken() {
	if a.take() {
		a.settle()
	}
}

// probeBody settles a held failed response when the SDK reads it: the call ends with it.
type probeBody struct {
	io.ReadCloser
	once sync.Once
	a    *attempt
}

func (b *probeBody) Read(p []byte) (int, error) {
	b.once.Do(func() { safe(b.a.settleIfTaken) })
	return b.ReadCloser.Read(p)
}

func (b *probeBody) Close() error {
	b.once.Do(func() { b.a.closed.Store(true) })
	return b.ReadCloser.Close()
}
