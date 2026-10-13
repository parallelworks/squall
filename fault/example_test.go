package fault_test

import (
	"fmt"

	"github.com/parallelworks/squall/fault"
)

func Example() {
	in := fault.New(fault.Seed(42)).Add(
		// The second insert takes effect, but its caller never hears back.
		fault.Rule{Method: "compute.addresses.insert", Nth: 2, Fault: fault.LostResponse()},
	)
	call := fault.Call{Method: "compute.addresses.insert", Resource: "projects/p/regions/us-east1/addresses"}
	for range 3 {
		in.Decide(call)
	}
	for _, i := range in.Injected() {
		fmt.Println(i)
	}
	// Output: #2 compute.addresses.insert projects/p/regions/us-east1/addresses: lose response
}
