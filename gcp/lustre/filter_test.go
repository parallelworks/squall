package lustre

import (
	"errors"
	"testing"
)

func TestParseFilter(t *testing.T) {
	for in, want := range map[string]string{
		"":                                              "",
		"labels.session-id=abc":                         "labels.session-id = abc",
		` state = "ACTIVE" `:                            "state = ACTIVE",
		"labels.owner:*":                                "labels.owner:*",
		"labels:owner":                                  "labels:owner",
		"state = ACTIVE labels.a = b":                   "(state = ACTIVE) AND (labels.a = b)",
		"state = ACTIVE AND labels.a != b":              "(state = ACTIVE) AND (labels.a != b)",
		"state = CREATING OR state = ACTIVE":            "(state = CREATING) OR (state = ACTIVE)",
		"labels.a = x AND state = A OR state = B":       "(labels.a = x) AND ((state = A) OR (state = B))",
		"NOT state = DELETING":                          "NOT (state = DELETING)",
		"-labels.keep:*":                                "NOT (labels.keep:*)",
		"(labels.a = x OR labels.b = y) state = ACTIVE": "((labels.a = x) OR (labels.b = y)) AND (state = ACTIVE)",
		`description = "two words"`:                     "description = two words",
	} {
		got, err := parseFilter(in)
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
		"capacityGib > 9000",
		"capacityGib = 9000",
		"labels. = x",
		"labels = x",
		"state =",
		`state = "open`,
		`name = "a\"b"`,
		"(state = A",
		"state = A)",
		"state = A AND",
		"state = A OR",
		"NOT",
		"name = inst-*",
		"labels.a:x",
		"labels:a*",
		"state < ACTIVE",
		"has(labels.a)",
	} {
		if _, err := parseFilter(in); !errors.Is(err, errUnsupportedFilter) {
			t.Errorf("parseFilter(%q) = %v, want errUnsupportedFilter", in, err)
		}
	}
}

func TestFilterMatches(t *testing.T) {
	in := &Instance{
		Name:   "projects/p/locations/z/instances/fs-1",
		State:  StateActive,
		Labels: map[string]string{"owner": "me", "empty": ""},
	}
	for filter, want := range map[string]bool{
		"name = projects/p/locations/z/instances/fs-1": true,
		"state = ACTIVE":                       true,
		"state != ACTIVE":                      false,
		"labels.owner = me":                    true,
		"labels.owner = you":                   false,
		"labels.missing = me":                  false,
		"labels.missing != me":                 true,
		"labels.owner:*":                       true,
		"labels.empty:*":                       true,
		"labels.missing:*":                     false,
		"labels:owner":                         true,
		"labels:missing":                       false,
		"-labels:owner":                        false,
		"NOT labels.missing:*":                 true,
		"labels.owner = me state = CREATING":   false,
		"labels.owner = you OR state = ACTIVE": true,
		"description:*":                        false,
		`description = ""`:                     true,
	} {
		e, err := parseFilter(filter)
		if err != nil {
			t.Fatalf("parseFilter(%q): %v", filter, err)
		}
		if got := e.matches(in); got != want {
			t.Errorf("%q matches = %v, want %v", filter, got, want)
		}
	}
}

func FuzzParseFilter(f *testing.F) {
	for _, seed := range []string{"labels.a = b", "state = ACTIVE OR NOT (labels:x)", `name = "x y"`, "-labels.a:*"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := parseFilter(s)
		if err != nil && !errors.Is(err, errUnsupportedFilter) {
			t.Errorf("parseFilter(%q) = %v, want errUnsupportedFilter", s, err)
		}
		e.matches(&Instance{Labels: map[string]string{"a": "b"}})
	})
}
