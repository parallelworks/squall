package fault_test

import (
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/parallelworks/squall/fault"
)

var insert = fault.Call{Method: "compute.addresses.insert", Resource: "projects/p/regions/r/addresses"}

func decide(in *fault.Injector, c fault.Call, n int) []bool {
	out := make([]bool, n)
	for i := range n {
		_, out[i] = in.Decide(c)
	}
	return out
}

func TestNilInjectorInjectsNothing(t *testing.T) {
	var in *fault.Injector
	if f, ok := in.Decide(insert); ok {
		t.Fatalf("Decide = %v, want no fault", f)
	}
	if got := in.Injected(); got != nil {
		t.Fatalf("Injected = %v", got)
	}
}

func TestRuleNthFiresOnce(t *testing.T) {
	in := fault.New().Add(fault.Rule{Method: insert.Method, Nth: 2, Fault: fault.LostResponse()})
	if got, want := decide(in, insert, 4), []bool{false, true, false, false}; !slices.Equal(got, want) {
		t.Fatalf("decisions = %v, want %v", got, want)
	}
	got := in.Injected()
	if len(got) != 1 || got[0].Seq != 2 || got[0].Fault.Mode != fault.LoseResponse {
		t.Fatalf("Injected = %v", got)
	}
}

func TestRuleTimesLimitsFiring(t *testing.T) {
	in := fault.New().Add(fault.Rule{Method: "compute.addresses.*", Times: 2, Fault: fault.Unavailable()})
	if got, want := decide(in, insert, 3), []bool{true, true, false}; !slices.Equal(got, want) {
		t.Fatalf("decisions = %v, want %v", got, want)
	}
}

func TestRuleMatching(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule fault.Rule
		want bool
	}{
		{"every method", fault.Rule{}, true},
		{"exact method", fault.Rule{Method: insert.Method}, true},
		{"other method", fault.Rule{Method: "compute.addresses.delete"}, false},
		{"prefix", fault.Rule{Method: "compute.*"}, true},
		{"other prefix", fault.Rule{Method: "storage.*"}, false},
		{"resource", fault.Rule{Resource: insert.Resource}, true},
		{"other resource", fault.Rule{Resource: "projects/q/regions/r/addresses"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.rule.Fault = fault.Unavailable()
			if _, got := fault.New().Add(tc.rule).Decide(insert); got != tc.want {
				t.Fatalf("Decide fired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFirstFiringRuleWins(t *testing.T) {
	in := fault.New().Add(
		fault.Rule{Method: insert.Method, Nth: 1, Fault: fault.LostResponse()},
		fault.Rule{Method: insert.Method, Fault: fault.Unavailable()},
	)
	first, _ := in.Decide(insert)
	second, _ := in.Decide(insert)
	if first.Mode != fault.LoseResponse || second.Mode != fault.Reject {
		t.Fatalf("faults = %v, %v", first, second)
	}
}

func TestRandomIsReproducibleFromItsSeed(t *testing.T) {
	run := func(seed uint64) []bool {
		return decide(fault.New(fault.Seed(seed)).Random(0.3, fault.Unavailable(), fault.LostResponse()), insert, 200)
	}
	a, b := run(7), run(7)
	if !slices.Equal(a, b) {
		t.Fatal("the same seed made different decisions")
	}
	if slices.Equal(a, run(8)) {
		t.Fatal("different seeds made the same 200 decisions")
	}
	n := 0
	for _, failed := range a {
		if failed {
			n++
		}
	}
	if n < 30 || n > 90 {
		t.Fatalf("%d of 200 calls failed at rate 0.3", n)
	}
}

func TestRulesTakePrecedenceOverRandom(t *testing.T) {
	in := fault.New().Add(fault.Rule{Method: insert.Method, Fault: fault.Slow(time.Second)}).Random(1, fault.Unavailable())
	if f, ok := in.Decide(insert); !ok || f.Mode != fault.Pass || f.Delay != time.Second {
		t.Fatalf("Decide = %v, %v", f, ok)
	}
}

func TestDecideIsSafeForConcurrentUse(t *testing.T) {
	in := fault.New().Add(fault.Rule{Method: insert.Method, Times: 50, Fault: fault.Unavailable()})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { decide(in, insert, 25) })
	}
	wg.Wait()
	if got := len(in.Injected()); got != 50 {
		t.Fatalf("%d faults injected, want 50", got)
	}
}

func TestFaultString(t *testing.T) {
	for _, tc := range []struct {
		f    fault.Fault
		want string
	}{
		{fault.Unavailable(), "reject 503 backendError"},
		{fault.LostResponse(), "lose response"},
		{fault.Slow(2 * time.Second), "pass after 2s"},
		{fault.OperationFailed(http.StatusInternalServerError), "fail operation 500 backendError"},
		{fault.Fault{Mode: 99}, "Mode(99)"},
	} {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}
