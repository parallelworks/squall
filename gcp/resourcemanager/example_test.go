package resourcemanager_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/resourcemanager"
)

// The first setIamPolicy takes effect but its response is lost. Retrying with
// the etag read before it fails with ABORTED, so code under test must read
// the policy again and find its change already made. Real tests point the
// official client library at srv.URL with option.WithEndpoint.
func Example() {
	faults := fault.New().Add(fault.Rule{Method: "cloudresourcemanager.projects.setIamPolicy", Nth: 1, Fault: fault.LoseResponse()})
	s := resourcemanager.New(resourcemanager.WithFaults(faults), resourcemanager.WithProject("my-project", 123))
	srv := httptest.NewServer(s)
	defer srv.Close()

	before, _ := s.IamPolicy("my-project")
	set := `{"policy":{"bindings":[{"role":"roles/viewer","members":["user:a@example.com"]}],"etag":"` + before.Etag + `"}}`
	for range 2 {
		resp, err := http.Post(srv.URL+"/v3/projects/my-project:setIamPolicy", "application/json", strings.NewReader(set))
		if err != nil {
			fmt.Println("set failed:", err)
			continue
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Println("set:", resp.StatusCode, "lost:", err != nil)
	}

	after, _ := s.IamPolicy("123")
	fmt.Println(after.Bindings[0].Role, after.Bindings[0].Members)
	// Output:
	// set: 200 lost: true
	// set: 409 lost: false
	// roles/viewer [user:a@example.com]
}
