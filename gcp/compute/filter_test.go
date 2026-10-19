package compute

import (
	"errors"
	"testing"
)

func TestParseFilter(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "",
		"labels.session-id=abc":                 "labels.session-id = abc",
		"  name != ip-1 ":                       "name != ip-1",
		`description = "two words"`:             "description = two words",
		"(name = a) (status = RESERVED)":        "(name = a) AND (status = RESERVED)",
		"(name = a) AND (labels.k != v)":        "(name = a) AND (labels.k != v)",
		"name = a AND addressType = INTERNAL":   "(name = a) AND (addressType = INTERNAL)",
		"( name = a ) AND ( status = b )":       "(name = a) AND (status = b)",
		"id=1 OR id=2 OR id=3":                  "(id = 1) OR (id = 2) OR (id = 3)",
		"labels.s=x AND (name=a OR name=b)":     "(labels.s = x) AND ((name = a) OR (name = b))",
		"name = a AND status = b OR status = c": "(name = a) AND ((status = b) OR (status = c))",
		"(name = a) OR (name = b) (status = c)": "((name = a) OR (name = b)) AND (status = c)",
		"name eq ip-.*":                         "name eq ip-.*",
		"name eq (a|b)-[0-9]+":                  "name eq (a|b)-[0-9]+",
		"name ne 'ip-(1|2)'":                    "name ne ip-(1|2)",
		`(name eq "a.*") (labels.k ne (x|y)z)`:  "(name eq a.*) AND (labels.k ne (x|y)z)",
		"labels.session-id=\"x\" AND id != 7":   "(labels.session-id = x) AND (id != 7)",
	} {
		got, err := parseFilter(in, addressFields)
		if err != nil {
			t.Errorf("parseFilter(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("parseFilter(%q) = %q, want %q", in, got, want)
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
		"name = a)",
		"name = a ANDstatus = b",
		"name = a AND",
		"name = a AND ",
		"name = a OR",
		"name = ip-*",
		`description = "*"`,
		"name eq a.* AND status eq b",
		"name eq a status = b",
		"(name = a) (status eq b)",
		"name eq (",
		"name eq ''",
		"(name eq a) status eq b",
		"NOT name = a",
		"name eq unquoted literal",
		"name eq a name eq b",
	} {
		if _, err := parseFilter(in, addressFields); !errors.Is(err, errUnsupportedFilter) {
			t.Errorf("parseFilter(%q) = %v, want errUnsupportedFilter", in, err)
		}
	}
}

func TestFilterMatches(t *testing.T) {
	a := &Address{ID: 42, Name: "ip-1", Status: "RESERVED", Labels: map[string]string{"owner": "me"}}
	for in, want := range map[string]bool{
		"name = ip-1":                        true,
		"name != ip-1":                       false,
		"labels.owner = me":                  true,
		"labels.missing = me":                false,
		"labels.missing != me":               true,
		"(name = ip-1) (status = RESERVING)": false,
		"id = 42":                            true,
		"id = 7 OR id = 42":                  true,
		"name = ip-2 OR name = ip-3":         false,
		"labels.owner = me AND (name = ip-2 OR name = ip-1)": true,
		"name eq ip-.*":     true,
		"name eq ip":        false,
		"name eq .*1":       true,
		"name ne .*1":       false,
		"labels.owner eq m": false,
	} {
		f, err := parseFilter(in, addressFields)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.matches(a); got != want {
			t.Errorf("%q matches = %v, want %v", in, got, want)
		}
	}
}

func FuzzParseFilter(f *testing.F) {
	for _, seed := range []string{"labels.a=b", `(name = "x y") AND (status != DONE)`, "name=a addressType=b", "((", `"`, "id=1 OR id=2", "name eq a.*", "(name ne 'x')(id eq 1)"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		parsed, err := parseFilter(in, addressFields)
		if err != nil {
			if !errors.Is(err, errUnsupportedFilter) {
				t.Fatalf("parseFilter(%q) = %v, not an errUnsupportedFilter", in, err)
			}
			return
		}
		parsed.comparisons(func(c comparison) {
			if !addressFields.has(c.field) {
				t.Fatalf("parseFilter(%q) accepted field %q", in, c.field)
			}
		})
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
