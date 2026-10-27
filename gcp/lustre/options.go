package lustre

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Enum names, indexed by their numbers in the API.
var (
	squashModes = []string{"SQUASH_MODE_UNSPECIFIED", "NO_SQUASH", "ROOT_SQUASH"}
	tierModes   = []string{"MODE_UNSPECIFIED", "DISABLED", "DEFAULT_CACHE"}
	daysOfWeek  = []string{"DAY_OF_WEEK_UNSPECIFIED", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}
)

// readEnum takes a name or a number, since the client sends enums as numbers.
func readEnum(b []byte, names []string, into *string) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		if !slices.Contains(names, name) {
			return fmt.Errorf("invalid value %s, want one of %v", b, names)
		}
		*into = name
		return nil
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 || n >= len(names) {
		return fmt.Errorf("invalid value %s, want one of %v", b, names)
	}
	*into = names[n]
	return nil
}

// SquashMode is a root squash mode, such as "ROOT_SQUASH".
type SquashMode string

// UnmarshalJSON reads m from its name or number.
func (m *SquashMode) UnmarshalJSON(b []byte) error { return readEnum(b, squashModes, (*string)(m)) }

// TierMode is a Dynamic tier mode, such as "DEFAULT_CACHE".
type TierMode string

// UnmarshalJSON reads m from its name or number.
func (m *TierMode) UnmarshalJSON(b []byte) error { return readEnum(b, tierModes, (*string)(m)) }

// DayOfWeek is a google.type.DayOfWeek, such as "MONDAY".
type DayOfWeek string

// UnmarshalJSON reads d from its name or number.
func (d *DayOfWeek) UnmarshalJSON(b []byte) error { return readEnum(b, daysOfWeek, (*string)(d)) }

// AccessRulesOptions is an instance's root squash configuration.
type AccessRulesOptions struct {
	AccessRules       []AccessRule `json:"accessRules,omitempty"`
	DefaultSquashMode SquashMode   `json:"defaultSquashMode,omitzero"`
	DefaultSquashUID  int32        `json:"defaultSquashUid,omitzero"`
	DefaultSquashGID  int32        `json:"defaultSquashGid,omitzero"`
}

// UnmarshalJSON reads o, taking the squash IDs as JSON strings or numbers.
func (o *AccessRulesOptions) UnmarshalJSON(b []byte) error {
	var in struct {
		AccessRules       []AccessRule `json:"accessRules"`
		DefaultSquashMode SquashMode   `json:"defaultSquashMode"`
		DefaultSquashUID  int32Value   `json:"defaultSquashUid"`
		DefaultSquashGID  int32Value   `json:"defaultSquashGid"`
	}
	if err := strictly(b, &in); err != nil {
		return err
	}
	*o = AccessRulesOptions{in.AccessRules, in.DefaultSquashMode, int32(in.DefaultSquashUID), int32(in.DefaultSquashGID)}
	return nil
}

// int32Value reads proto3 JSON, which spells an int32 as a string or a number.
type int32Value int32

func (n *int32Value) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 32)
	if err != nil {
		return fmt.Errorf("invalid int32 value %s", b)
	}
	*n = int32Value(v)
	return nil
}

// AccessRule is a group of client addresses with their own squash mode.
type AccessRule struct {
	Name            string     `json:"name,omitzero"`
	IPAddressRanges []string   `json:"ipAddressRanges,omitempty"`
	SquashMode      SquashMode `json:"squashMode,omitzero"`
}

// DynamicTierOptions chooses the Dynamic tier.
type DynamicTierOptions struct {
	Mode TierMode `json:"mode,omitzero"`
}

// MaintenancePolicy is when an instance may and may not be maintained.
type MaintenancePolicy struct {
	WeeklyMaintenanceWindows   []WeeklyMaintenanceWindow    `json:"weeklyMaintenanceWindows,omitempty"`
	MaintenanceExclusionWindow []MaintenanceExclusionWindow `json:"maintenanceExclusionWindow,omitempty"`
}

// WeeklyMaintenanceWindow is a weekly hour in which maintenance may happen.
type WeeklyMaintenanceWindow struct {
	DayOfWeek DayOfWeek  `json:"dayOfWeek,omitzero"`
	StartTime *TimeOfDay `json:"startTime,omitzero"`
}

// MaintenanceExclusionWindow is a period in which no maintenance happens.
type MaintenanceExclusionWindow struct {
	StartDate *Date      `json:"startDate,omitzero"`
	EndDate   *Date      `json:"endDate,omitzero"`
	Time      *TimeOfDay `json:"time,omitzero"`
}

// TimeOfDay is a google.type.TimeOfDay.
type TimeOfDay struct {
	Hours   int32 `json:"hours,omitzero"`
	Minutes int32 `json:"minutes,omitzero"`
	Seconds int32 `json:"seconds,omitzero"`
	Nanos   int32 `json:"nanos,omitzero"`
}

// Date is a google.type.Date.
type Date struct {
	Year  int32 `json:"year,omitzero"`
	Month int32 `json:"month,omitzero"`
	Day   int32 `json:"day,omitzero"`
}

// maintenanceSchedule is output only, decoded so its unknown fields are rejected.
type maintenanceSchedule struct {
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
}

func (o *AccessRulesOptions) check() *apiError {
	if o == nil {
		return nil
	}
	if o.DefaultSquashMode != "NO_SQUASH" && o.DefaultSquashMode != "ROOT_SQUASH" {
		return invalid("access_rules_options.default_squash_mode is required and must be NO_SQUASH or ROOT_SQUASH.")
	}
	if o.DefaultSquashMode == "NO_SQUASH" && (o.DefaultSquashUID != 0 || o.DefaultSquashGID != 0) {
		return invalid("access_rules_options.default_squash_uid and default_squash_gid must not be set when default_squash_mode is NO_SQUASH.")
	}
	for _, rule := range o.AccessRules {
		if !ruleNamePattern.MatchString(rule.Name) {
			return invalid("Invalid access rule name %q: it must have at most 16 letters, digits and underscores.", rule.Name)
		}
		if err := checkRanges(rule.Name, rule.IPAddressRanges); err != nil {
			return err
		}
		if rule.SquashMode != "NO_SQUASH" {
			return invalid("Access rule %q: squash_mode must be NO_SQUASH; ROOT_SQUASH is not supported inside an access rule.", rule.Name)
		}
	}
	return nil
}

func checkRanges(rule string, ranges []string) *apiError {
	if len(ranges) == 0 {
		return invalid("Access rule %q needs ip_address_ranges.", rule)
	}
	var seen []netip.Prefix
	for _, r := range ranges {
		prefix, err := netip.ParsePrefix(r)
		if err != nil {
			addr, aerr := netip.ParseAddr(r)
			if aerr != nil {
				return invalid("Access rule %q: %q is neither a CIDR range nor an IP address.", rule, r)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefix = prefix.Masked()
		for _, other := range seen {
			if prefix.Overlaps(other) {
				return invalid("Access rule %q: the ranges %s and %s overlap.", rule, other, prefix)
			}
		}
		seen = append(seen, prefix)
	}
	return nil
}

func (p *MaintenancePolicy) check() *apiError {
	if p == nil {
		return nil
	}
	if len(p.WeeklyMaintenanceWindows) != 1 {
		return invalid("maintenance_policy.weekly_maintenance_windows is required and limited to 1 window.")
	}
	if len(p.MaintenanceExclusionWindow) > 1 {
		return invalid("maintenance_policy.maintenance_exclusion_window is limited to 1 window.")
	}
	for _, w := range p.WeeklyMaintenanceWindows {
		if w.DayOfWeek == "" || w.DayOfWeek == DayOfWeek(daysOfWeek[0]) || w.StartTime == nil {
			return invalid("A weekly maintenance window needs day_of_week and start_time.")
		}
		if err := w.StartTime.check(); err != nil {
			return err
		}
	}
	for _, w := range p.MaintenanceExclusionWindow {
		if w.StartDate == nil || w.EndDate == nil || w.Time == nil {
			return invalid("A maintenance exclusion window needs start_date, end_date and time.")
		}
		for _, d := range []*Date{w.StartDate, w.EndDate} {
			if err := d.check(); err != nil {
				return err
			}
		}
		if err := w.Time.check(); err != nil {
			return err
		}
	}
	return nil
}

func (t *TimeOfDay) check() *apiError {
	if t.Hours < 0 || t.Hours > 23 || t.Minutes < 0 || t.Minutes > 59 || t.Seconds < 0 || t.Seconds > 59 || t.Nanos < 0 || t.Nanos > 999_999_999 {
		return invalid("Invalid time of day %+v.", *t)
	}
	return nil
}

// check takes a year of 0 for a window that recurs every year.
func (d *Date) check() *apiError {
	if d.Year < 0 || d.Year > 9999 || d.Month < 1 || d.Month > 12 || d.Day < 1 || d.Day > 31 {
		return invalid("Invalid date %+v: it needs a month and day, and a year from 1 to 9999 unless it recurs.", *d)
	}
	return nil
}

func (m *DynamicTierOptions) dynamic() bool { return m != nil && m.Mode == "DEFAULT_CACHE" }

func strictly(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

var enumPaths = []struct {
	path  []string
	names []string
}{
	{[]string{"state"}, states},
	{[]string{"accessRulesOptions", "defaultSquashMode"}, squashModes},
	{[]string{"accessRulesOptions", "accessRules", "squashMode"}, squashModes},
	{[]string{"dynamicTierOptions", "mode"}, tierModes},
	{[]string{"maintenancePolicy", "weeklyMaintenanceWindows", "dayOfWeek"}, daysOfWeek},
}

func numberEnums(v map[string]any) {
	for _, e := range enumPaths {
		numberAt(v, e.path, e.names)
	}
}

func numberAt(v any, path []string, names []string) {
	switch v := v.(type) {
	case []any:
		for _, item := range v {
			numberAt(item, path, names)
		}
	case map[string]any:
		if len(path) == 1 {
			if name, ok := v[path[0]].(string); ok {
				v[path[0]] = max(slices.Index(names, name), 0)
			}
			return
		}
		numberAt(v[path[0]], path[1:], names)
	}
}
