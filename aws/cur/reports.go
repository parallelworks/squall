package cur

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	pageSize   = 5
	maxTags    = 200
	maxNameLen = 256
)

var (
	timeUnits        = []string{"HOURLY", "DAILY", "MONTHLY"}
	formats          = []string{"textORcsv", "Parquet"}
	compressions     = []string{"ZIP", "GZIP", "Parquet"}
	schemaElements   = []string{"RESOURCES", "SPLIT_COST_ALLOCATION_DATA", "MANUAL_DISCOUNT_COMPATIBILITY"}
	artifacts        = []string{"REDSHIFT", "QUICKSIGHT", "ATHENA"}
	reportVersioning = []string{"CREATE_NEW_REPORT", "OVERWRITE_REPORT"}
	regions          = []string{
		"af-south-1", "ap-east-1", "ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2", "ap-southeast-3",
		"ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ca-central-1", "eu-central-1", "eu-central-2", "eu-west-1",
		"eu-west-2", "eu-west-3", "eu-north-1", "eu-south-1", "eu-south-2", "me-central-1", "me-south-1", "sa-east-1",
		"us-east-1", "us-east-2", "us-west-1", "us-west-2", "cn-north-1", "cn-northwest-1",
	}

	namePattern        = `[0-9A-Za-z!\-_.*\'()]+`
	bucketPattern      = `[A-Za-z0-9_\.\-]+`
	prefixPattern      = `[0-9A-Za-z!\-_.*\'()/]*`
	billingViewPattern = `(arn:aws(-cn)?:billing::[0-9]{12}:billingview/)?[a-zA-Z0-9_\+=\.\-@].{1,30}`
	markerPattern      = `[A-Za-z0-9_\.\-=]*`

	nameRE        = anchored(namePattern)
	bucketRE      = anchored(bucketPattern)
	prefixRE      = anchored(prefixPattern)
	billingViewRE = anchored(billingViewPattern)
	markerRE      = anchored(markerPattern)
)

func anchored(pattern string) *regexp.Regexp { return regexp.MustCompile(`^(?:` + pattern + `)$`) }

// ReportDefinition is a Cost and Usage Report definition.
type ReportDefinition struct {
	ReportName string
	// TimeUnit is HOURLY, DAILY or MONTHLY.
	TimeUnit string
	// Format is textORcsv or Parquet.
	Format string
	// Compression is ZIP, GZIP or Parquet.
	Compression string
	// AdditionalSchemaElements holds RESOURCES, SPLIT_COST_ALLOCATION_DATA and MANUAL_DISCOUNT_COMPATIBILITY, as sent.
	AdditionalSchemaElements []string
	S3Bucket                 string
	S3Prefix                 string
	S3Region                 string
	AdditionalArtifacts      []string
	// RefreshClosedReports is nil when the definition did not set it.
	RefreshClosedReports *bool
	ReportVersioning     string
	BillingViewArn       string
	// Tags is nil when the definition was created without tags.
	Tags map[string]string
}

func (d *ReportDefinition) clone() ReportDefinition {
	c := *d
	c.AdditionalSchemaElements = slices.Clone(d.AdditionalSchemaElements)
	c.AdditionalArtifacts = slices.Clone(d.AdditionalArtifacts)
	if d.RefreshClosedReports != nil {
		c.RefreshClosedReports = new(*d.RefreshClosedReports)
	}
	c.Tags = maps.Clone(d.Tags)
	return c
}

type reportDefinitionJSON struct {
	ReportName               *string         `json:"ReportName,omitempty"`
	TimeUnit                 *string         `json:"TimeUnit,omitempty"`
	Format                   *string         `json:"Format,omitempty"`
	Compression              *string         `json:"Compression,omitempty"`
	AdditionalSchemaElements []string        `json:"AdditionalSchemaElements"`
	S3Bucket                 *string         `json:"S3Bucket,omitempty"`
	S3Prefix                 *string         `json:"S3Prefix,omitempty"`
	S3Region                 *string         `json:"S3Region,omitempty"`
	AdditionalArtifacts      []string        `json:"AdditionalArtifacts,omitempty"`
	RefreshClosedReports     *bool           `json:"RefreshClosedReports,omitempty"`
	ReportVersioning         *string         `json:"ReportVersioning,omitempty"`
	BillingViewArn           *string         `json:"BillingViewArn,omitempty"`
	ReportStatus             json.RawMessage `json:"ReportStatus,omitempty"`
}

type tagJSON struct {
	Key   *string `json:"Key"`
	Value *string `json:"Value"`
}

type putInput struct {
	ReportDefinition *reportDefinitionJSON `json:"ReportDefinition"`
	Tags             []tagJSON             `json:"Tags"`
}

type describeInput struct {
	MaxResults *int    `json:"MaxResults"`
	NextToken  *string `json:"NextToken"`
}

type describeOutput struct {
	ReportDefinitions []reportDefinitionJSON `json:"ReportDefinitions"`
	NextToken         string                 `json:"NextToken,omitempty"`
}

type deleteInput struct {
	ReportName *string `json:"ReportName"`
}

type deleteOutput struct {
	ResponseMessage string `json:"ResponseMessage"`
}

// violations collects constraint violations in the form AWS services report them.
type violations []string

func (v *violations) add(value *string, path, constraint string) {
	shown := "null"
	if value != nil {
		shown = "'" + *value + "'"
	}
	*v = append(*v, fmt.Sprintf("Value %s at '%s' failed to satisfy constraint: Member must %s", shown, path, constraint))
}

func (v *violations) required(value *string, path string) bool {
	if value == nil {
		v.add(nil, path, "not be null")
		return false
	}
	return true
}

func (v *violations) pattern(value *string, path, pattern string, re *regexp.Regexp, maxLen int, required bool) {
	if value == nil {
		if required {
			v.add(nil, path, "not be null")
		}
		return
	}
	if len(*value) > maxLen {
		v.add(value, path, fmt.Sprintf("have length less than or equal to %d", maxLen))
	}
	if !re.MatchString(*value) {
		v.add(value, path, "satisfy regular expression pattern: "+pattern)
	}
}

func (v *violations) enum(value *string, path string, allowed []string, required bool) {
	if value == nil {
		if required {
			v.add(nil, path, "not be null")
		}
		return
	}
	if !slices.Contains(allowed, *value) {
		v.add(value, path, "satisfy enum value set: ["+strings.Join(allowed, ", ")+"]")
	}
}

func (v *violations) enumList(values []string, path string, allowed []string) {
	for i, value := range values {
		if !slices.Contains(allowed, value) {
			*v = append(*v, fmt.Sprintf("Value '%s' at '%s.%d.member' failed to satisfy constraint: Member must satisfy enum value set: [%s]",
				value, path, i+1, strings.Join(allowed, ", ")))
		}
	}
}

// validate checks a report definition and its tags against the API reference's constraints.
func validate(d *reportDefinitionJSON, tags []tagJSON) *apiError {
	var v violations
	if d == nil {
		v.add(nil, "reportDefinition", "not be null")
		return constraintErrors(v)
	}
	const p = "reportDefinition."
	v.pattern(d.ReportName, p+"reportName", namePattern, nameRE, maxNameLen, true)
	v.enum(d.TimeUnit, p+"timeUnit", timeUnits, true)
	v.enum(d.Format, p+"format", formats, true)
	v.enum(d.Compression, p+"compression", compressions, true)
	if d.AdditionalSchemaElements == nil {
		v.add(nil, p+"additionalSchemaElements", "not be null")
	}
	v.enumList(d.AdditionalSchemaElements, p+"additionalSchemaElements", schemaElements)
	v.pattern(d.S3Bucket, p+"s3Bucket", bucketPattern, bucketRE, maxNameLen, true)
	v.pattern(d.S3Prefix, p+"s3Prefix", prefixPattern, prefixRE, maxNameLen, true)
	v.enum(d.S3Region, p+"s3Region", regions, true)
	v.enumList(d.AdditionalArtifacts, p+"additionalArtifacts", artifacts)
	v.enum(d.ReportVersioning, p+"reportVersioning", reportVersioning, false)
	v.pattern(d.BillingViewArn, p+"billingViewArn", billingViewPattern, billingViewRE, 128, false)
	if len(tags) > maxTags {
		v = append(v, fmt.Sprintf("Value at 'tags' failed to satisfy constraint: Member must have length less than or equal to %d", maxTags))
	}
	for i, t := range tags {
		path := fmt.Sprintf("tags.%d.member.", i+1)
		if v.required(t.Key, path+"key") && (len(*t.Key) < 1 || len(*t.Key) > 128) {
			v.add(t.Key, path+"key", "have length between 1 and 128")
		}
		if v.required(t.Value, path+"value") && len(*t.Value) > 256 {
			v.add(t.Value, path+"value", "have length less than or equal to 256")
		}
	}
	if len(v) > 0 {
		return constraintErrors(v)
	}
	switch {
	case *d.Format == "Parquet" && *d.Compression != "Parquet":
		return validationError("Parquet format requires Parquet compression.")
	case *d.Format == "textORcsv" && *d.Compression == "Parquet":
		return validationError("textORcsv format requires ZIP or GZIP compression.")
	}
	return nil
}

func (s *Server) putReportDefinition(in any) (any, *apiError) {
	req := in.(*putInput)
	if err := validate(req.ReportDefinition, req.Tags); err != nil {
		return nil, err
	}
	d := req.ReportDefinition
	def := &ReportDefinition{
		ReportName:               *d.ReportName,
		TimeUnit:                 *d.TimeUnit,
		Format:                   *d.Format,
		Compression:              *d.Compression,
		AdditionalSchemaElements: slices.Clone(d.AdditionalSchemaElements),
		S3Bucket:                 *d.S3Bucket,
		S3Prefix:                 *d.S3Prefix,
		S3Region:                 *d.S3Region,
		AdditionalArtifacts:      slices.Clone(d.AdditionalArtifacts),
		RefreshClosedReports:     d.RefreshClosedReports,
	}
	if d.ReportVersioning != nil {
		def.ReportVersioning = *d.ReportVersioning
	}
	if d.BillingViewArn != nil {
		def.BillingViewArn = *d.BillingViewArn
	}
	for _, t := range req.Tags {
		if def.Tags == nil {
			def.Tags = map[string]string{}
		}
		def.Tags[*t.Key] = *t.Value
	}

	if s.buckets != nil {
		policy, exists := s.buckets(def.S3Bucket)
		if !exists || !deliveryAllowed(policy, def.S3Bucket) {
			return nil, bucketPermission()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reports[def.ReportName]; ok {
		return nil, duplicateReportName(def.ReportName)
	}
	if len(s.reports) >= s.limit {
		return nil, reportLimitReached(s.limit)
	}
	s.reports[def.ReportName] = def
	return struct{}{}, nil
}

func (s *Server) describeReportDefinitions(in any) (any, *apiError) {
	req := in.(*describeInput)
	var v violations
	if req.MaxResults != nil && *req.MaxResults != pageSize {
		shown := strconv.Itoa(*req.MaxResults)
		constraint := "have value greater than or equal to 5"
		if *req.MaxResults > pageSize {
			constraint = "have value less than or equal to 5"
		}
		v.add(&shown, "maxResults", constraint)
	}
	after := ""
	if req.NextToken != nil {
		v.pattern(req.NextToken, "nextToken", markerPattern, markerRE, maxNameLen, false)
		if len(v) == 0 && *req.NextToken != "" {
			name, err := base64.RawURLEncoding.DecodeString(*req.NextToken)
			if err != nil || len(name) == 0 {
				return nil, validationError("The NextToken %s is not valid.", *req.NextToken)
			}
			after = string(name)
		}
	}
	if len(v) > 0 {
		return nil, constraintErrors(v)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	names := slices.Sorted(maps.Keys(s.reports))
	start, _ := slices.BinarySearch(names, after)
	if start < len(names) && names[start] == after {
		start++
	}
	out := describeOutput{ReportDefinitions: []reportDefinitionJSON{}}
	end := min(start+pageSize, len(names))
	for _, name := range names[start:end] {
		out.ReportDefinitions = append(out.ReportDefinitions, toJSON(s.reports[name]))
	}
	if end < len(names) {
		out.NextToken = base64.RawURLEncoding.EncodeToString([]byte(names[end-1]))
	}
	return out, nil
}

func toJSON(d *ReportDefinition) reportDefinitionJSON {
	c := d.clone()
	out := reportDefinitionJSON{
		ReportName:               &c.ReportName,
		TimeUnit:                 &c.TimeUnit,
		Format:                   &c.Format,
		Compression:              &c.Compression,
		AdditionalSchemaElements: c.AdditionalSchemaElements,
		S3Bucket:                 &c.S3Bucket,
		S3Prefix:                 &c.S3Prefix,
		S3Region:                 &c.S3Region,
		AdditionalArtifacts:      c.AdditionalArtifacts,
		RefreshClosedReports:     c.RefreshClosedReports,
	}
	if c.ReportVersioning != "" {
		out.ReportVersioning = &c.ReportVersioning
	}
	if c.BillingViewArn != "" {
		out.BillingViewArn = &c.BillingViewArn
	}
	return out
}

func (s *Server) deleteReportDefinition(in any) (any, *apiError) {
	req := in.(*deleteInput)
	var v violations
	v.pattern(req.ReportName, "reportName", namePattern, nameRE, maxNameLen, true)
	if len(v) > 0 {
		return nil, constraintErrors(v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reports, *req.ReportName)
	return deleteOutput{ResponseMessage: "Deleted report definition " + *req.ReportName}, nil
}

// ReportDefinitions returns every report definition, ordered by name.
func (s *Server) ReportDefinitions() []ReportDefinition {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ReportDefinition, 0, len(s.reports))
	for _, name := range slices.Sorted(maps.Keys(s.reports)) {
		out = append(out, s.reports[name].clone())
	}
	return out
}
