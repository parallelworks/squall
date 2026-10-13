// Package fault decides which API calls a stand-in server fails, and how.
//
// An [Injector] holds scripted [Rule]s, such as "lose the response to the
// second insert", and an optional seeded random rate. Servers ask it about
// every call with [Injector.Decide]. Given the same seed and the same order of
// calls, it makes the same decisions, so a failure found by a random run
// replays exactly.
package fault

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Call is one API request as the injector sees it.
type Call struct {
	// Method is the API method ID, such as "compute.addresses.insert".
	Method string
	// Resource names what the call acts on, such as
	// "projects/p/regions/r/addresses/a", or the collection for a list or insert.
	Resource string
}

// Mode is when a fault strikes relative to the call's effect.
type Mode uint8

const (
	// Pass handles the call normally, after the fault's Delay.
	Pass Mode = iota
	// Reject fails the call before it takes effect.
	Reject
	// LoseResponse applies the call, then closes the connection before the
	// response is complete, whether the call succeeded or not.
	LoseResponse
	// FailAfter applies the call, then answers with an error.
	FailAfter
	// FailOperation accepts the call and answers with a long-running operation
	// that ends in an error, without the call taking effect. A method that
	// starts no operation, such as a get, fails as Reject.
	FailOperation
)

var modeNames = [...]string{Pass: "pass", Reject: "reject", LoseResponse: "lose response", FailAfter: "fail after", FailOperation: "fail operation"}

func (m Mode) String() string {
	if int(m) < len(modeNames) {
		return modeNames[m]
	}
	return fmt.Sprintf("Mode(%d)", m)
}

// Fault is what the server does to one call.
type Fault struct {
	Mode Mode
	// Status is the HTTP status of the error, for Reject, FailAfter and FailOperation.
	Status int
	// Reason is the machine-readable error reason, such as "backendError".
	Reason string
	// Delay is how long the server waits before it handles the call.
	Delay time.Duration
}

func (f Fault) String() string {
	var b strings.Builder
	b.WriteString(f.Mode.String())
	if f.Status != 0 {
		fmt.Fprintf(&b, " %d", f.Status)
	}
	if f.Reason != "" {
		fmt.Fprintf(&b, " %s", f.Reason)
	}
	if f.Delay > 0 {
		fmt.Fprintf(&b, " after %s", f.Delay)
	}
	return b.String()
}

// Unavailable rejects the call as a transient server error.
func Unavailable() Fault {
	return Fault{Mode: Reject, Status: http.StatusServiceUnavailable, Reason: "backendError"}
}

// RateLimited rejects the call as over quota.
func RateLimited() Fault {
	return Fault{Mode: Reject, Status: http.StatusTooManyRequests, Reason: "rateLimitExceeded"}
}

// LostResponse applies the call and drops its response, as a timeout or a
// broken connection after the server committed would.
func LostResponse() Fault { return Fault{Mode: LoseResponse} }

// ErrorAfterApply applies the call and answers with status anyway.
func ErrorAfterApply(status int) Fault {
	return Fault{Mode: FailAfter, Status: status, Reason: "backendError"}
}

// OperationFailed accepts the call and ends its operation in an error with
// status, without the call taking effect.
func OperationFailed(status int) Fault {
	return Fault{Mode: FailOperation, Status: status, Reason: "backendError"}
}

// Slow delays the call by d and then handles it normally.
func Slow(d time.Duration) Fault { return Fault{Delay: d} }

// Rule scripts a fault for the calls it matches.
type Rule struct {
	// Method matches a method ID exactly, or every method with a prefix when it
	// ends in "*" ("compute.addresses.*"). Empty matches every method.
	Method string
	// Resource, when set, matches only calls on that resource.
	Resource string
	// Nth, when set, fires on the nth call the rule matches only (1-based),
	// counting calls an earlier rule decided.
	Nth int
	// Times limits how often the rule fires; 0 is unlimited. Ignored with Nth.
	Times int
	Fault Fault
}

func (r Rule) matches(c Call) bool {
	if r.Resource != "" && r.Resource != c.Resource {
		return false
	}
	if prefix, ok := strings.CutSuffix(r.Method, "*"); ok {
		return strings.HasPrefix(c.Method, prefix)
	}
	return r.Method == "" || r.Method == c.Method
}

type rule struct {
	Rule
	matched, fired int
}

func (r *rule) due() bool {
	if r.Nth > 0 {
		return r.matched == r.Nth
	}
	return r.Times == 0 || r.fired < r.Times
}

// Injected records one fault the injector handed out.
type Injected struct {
	// Seq is the call's 1-based position among every call decided.
	Seq   int
	Call  Call
	Fault Fault
}

func (i Injected) String() string {
	return fmt.Sprintf("#%d %s %s: %s", i.Seq, i.Call.Method, i.Call.Resource, i.Fault)
}

// Injector decides the fault for each call. A nil *Injector injects nothing,
// and the zero value is an injector with no rules and seed 0. It is safe for
// concurrent use; decisions are reproducible when calls arrive in the same
// order.
type Injector struct {
	mu       sync.Mutex
	seed     uint64
	rng      *rand.Rand
	rules    []*rule
	rate     float64
	random   []Fault
	calls    int
	injected []Injected
}

// Option configures an [Injector].
type Option func(*Injector)

// Seed sets the seed random faults are drawn from. The default is 0.
func Seed(seed uint64) Option {
	return func(in *Injector) { in.seed = seed }
}

// New returns an injector with no rules.
func New(opts ...Option) *Injector {
	in := &Injector{}
	for _, opt := range opts {
		opt(in)
	}
	return in
}

// Seed returns the seed random faults are drawn from, for reporting a failure.
func (in *Injector) Seed() uint64 {
	if in == nil {
		return 0
	}
	return in.seed
}

// Add appends rules. The first rule that fires decides a call.
func (in *Injector) Add(rules ...Rule) *Injector {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, r := range rules {
		in.rules = append(in.rules, &rule{Rule: r})
	}
	return in
}

// Random fails each call no rule decides with probability rate, using a fault
// drawn uniformly from faults.
func (in *Injector) Random(rate float64, faults ...Fault) *Injector {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.rate = rate
	in.random = slices.Clone(faults)
	return in
}

// Decide returns the fault for c, and false when the call is handled normally.
func (in *Injector) Decide(c Call) (Fault, bool) {
	if in == nil {
		return Fault{}, false
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.calls++
	f, ok := in.scripted(c)
	if !ok && len(in.random) > 0 {
		if in.rng == nil {
			in.rng = rand.New(rand.NewPCG(in.seed, 0x5eed))
		}
		if in.rng.Float64() < in.rate {
			f, ok = in.random[in.rng.IntN(len(in.random))], true
		}
	}
	if ok {
		in.injected = append(in.injected, Injected{Seq: in.calls, Call: c, Fault: f})
	}
	return f, ok
}

func (in *Injector) scripted(c Call) (Fault, bool) {
	var chosen *rule
	for _, r := range in.rules {
		if !r.matches(c) {
			continue
		}
		r.matched++
		if chosen == nil && r.due() {
			chosen = r
		}
	}
	if chosen == nil {
		return Fault{}, false
	}
	chosen.fired++
	return chosen.Fault, true
}

// Injected returns the faults handed out so far, in call order.
func (in *Injector) Injected() []Injected {
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]Injected(nil), in.injected...)
}
