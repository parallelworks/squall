package compute

import (
	"errors"
	"testing"
)

func TestParseFilter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want filter
	}{
		{"", nil},
		{"labels.session-id=abc", filter{{"labels.session-id", "abc", true}}},
		{"  name != ip-1 ", filter{{"name", "ip-1", false}}},
		{`description = "two words"`, filter{{"description", "two words", true}}},
		{"(name = a) (status = RESERVED)", filter{{"name", "a", true}, {"status", "RESERVED", true}}},
		{"(name = a) AND (labels.k != v)", filter{{"name", "a", true}, {"labels.k", "v", false}}},
		{"name = a AND addressType = INTERNAL", filter{{"name", "a", true}, {"addressType", "INTERNAL", true}}},
		{"( name = a ) AND ( status = b )", filter{{"name", "a", true}, {"status", "b", true}}},
	} {
		got, err := parseFilter(tc.in)
		if err != nil {
			t.Errorf("parseFilter(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseFilter(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseFilter(%q)[%d] = %v, want %v", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseFilterRejectsWhatItDoesNotSupport(t *testing.T) {
	for _, in := range []string{
		"creationTimestamp = 1",
		"labels. = x",
		"name > a",
		"name =",
		`name = "open`,
		"(name = a",
		"name = a ANDstatus = b",
		"name eq a.*",
		"name = a AND",
		"name = a AND ",
		"name = ip-*",
		`description = "*"`,
	} {
		if _, err := parseFilter(in); !errors.Is(err, errUnsupportedFilter) {
			t.Errorf("parseFilter(%q) = %v, want errUnsupportedFilter", in, err)
		}
	}
}

func TestFilterMatches(t *testing.T) {
	a := &Address{Name: "ip-1", Status: "RESERVED", Labels: map[string]string{"owner": "me"}}
	for in, want := range map[string]bool{
		"name = ip-1":                        true,
		"name != ip-1":                       false,
		"labels.owner = me":                  true,
		"labels.missing = me":                false,
		"labels.missing != me":               true,
		"(name = ip-1) (status = RESERVING)": false,
	} {
		f, err := parseFilter(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.matches(a); got != want {
			t.Errorf("%q matches = %v, want %v", in, got, want)
		}
	}
}

func FuzzParseFilter(f *testing.F) {
	for _, seed := range []string{"labels.a=b", `(name = "x y") AND (status != DONE)`, "name=a addressType=b", "((", `"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		parsed, err := parseFilter(in)
		if err != nil {
			if !errors.Is(err, errUnsupportedFilter) {
				t.Fatalf("parseFilter(%q) = %v, not an errUnsupportedFilter", in, err)
			}
			return
		}
		for _, c := range parsed {
			if !filterable(c.field) {
				t.Fatalf("parseFilter(%q) accepted field %q", in, c.field)
			}
		}
	})
}

func TestUpperSnake(t *testing.T) {
	for in, want := range map[string]string{
		"backendError":          "BACKEND_ERROR",
		"Internal Server Error": "INTERNAL_SERVER_ERROR",
		"rateLimitExceeded":     "RATE_LIMIT_EXCEEDED",
		"Forbidden":             "FORBIDDEN",
	} {
		if got := upperSnake(in); got != want {
			t.Errorf("upperSnake(%q) = %q, want %q", in, got, want)
		}
	}
}
