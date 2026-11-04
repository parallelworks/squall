package arm

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

// worm is a blob container's legal hold and time-based retention policy.
type worm struct {
	tags   []legalTag
	policy *retentionPolicy
}

type legalTag struct {
	tag string
	at  time.Time
}

type retentionPolicy struct {
	days                          int64
	appendWrites, appendWritesAll bool
	locked                        bool
	etag                          string
	history                       []map[string]any
}

func (w *worm) held() bool { return w != nil && len(w.tags) > 0 }

func (w *worm) locked() bool { return w != nil && w.policy != nil && w.policy.locked }

// protects reports whether the container's policy keeps e, made at created,
// from being deleted or overwritten at now.
func (w *worm) protects(created, now time.Time) bool {
	return w != nil && w.policy != nil && now.Before(created.AddDate(0, 0, int(w.policy.days)))
}

const immutabilityPolicyType = "Microsoft.Storage/storageAccounts/blobServices/containers/immutabilityPolicies"

func (s *Server) registerWormRoutes() {
	item := account + "/blobServices/default/containers/{name}"
	s.add("POST", item+"/setLegalHold", containerKind.action("setLegalHold/action"), false, s.legalHold(true))
	s.add("POST", item+"/clearLegalHold", containerKind.action("clearLegalHold/action"), false, s.legalHold(false))
	s.add("PUT", item+"/immutabilityPolicies/{policy}", containerKind.action("immutabilityPolicies/write"), false, s.putImmutabilityPolicy)
	s.add("POST", item+"/immutabilityPolicies/{policy}/lock", containerKind.action("immutabilityPolicies/lock/action"), false, s.lockImmutabilityPolicy)
}

// wormContainer returns the call's container for a change to its legal hold
// or policy, or the error the change answers. The caller holds s.mu.
func (s *Server) wormContainer(c *call) (*resource, *apiError) {
	if _, err := s.writableAccount(c, containerKind); err != nil {
		return nil, err
	}
	res := s.resources[key(containers.id(c))]
	if res == nil {
		return nil, containers.notFound()
	}
	if res.worm == nil {
		res.worm = &worm{}
	}
	return res, nil
}

func (s *Server) legalHold(set bool) handler {
	return func(c *call) (*reply, *apiError) {
		body, err := readObject(c.r)
		if err != nil {
			return nil, err
		}
		if err := checkMembers(body, "LegalHold", "", "tags", "allowProtectedAppendWritesAll", "hasLegalHold"); err != nil {
			return nil, err
		}
		raw, ok := body["tags"].([]any)
		if !ok {
			return nil, requiredParameter("tags")
		}
		var tags []string
		for _, t := range raw {
			tag, _ := t.(string)
			if len(tag) < 3 || len(tag) > 23 || strings.Trim(strings.ToLower(tag), "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
				return nil, invalidParameter("tags", "Each tag should be 3 to 23 alphanumeric characters.")
			}
			tags = append(tags, strings.ToLower(tag))
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		res, aerr := s.wormContainer(c)
		if aerr != nil {
			return nil, aerr
		}
		w := res.worm
		for _, tag := range tags {
			have := slices.ContainsFunc(w.tags, func(t legalTag) bool { return t.tag == tag })
			switch {
			case set && !have:
				w.tags = append(w.tags, legalTag{tag: tag, at: s.now()})
			case !set:
				w.tags = slices.DeleteFunc(w.tags, func(t legalTag) bool { return t.tag == tag })
			}
		}
		names := make([]string, 0, len(w.tags))
		for _, t := range w.tags {
			names = append(names, t.tag)
		}
		return ok200(map[string]any{"hasLegalHold": w.held(), "tags": names}), nil
	}
}

func (s *Server) putImmutabilityPolicy(c *call) (*reply, *apiError) {
	if c.val("policy") != "default" {
		return nil, unsupported("the only immutability policy is default")
	}
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	if err := checkMembers(body, "ImmutabilityPolicy", "", "id", "name", "type", "etag", "properties"); err != nil {
		return nil, err
	}
	props, err := object(body, "properties")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(props, "ImmutabilityPolicyProperty", "properties",
		"immutabilityPeriodSinceCreationInDays", "allowProtectedAppendWrites", "allowProtectedAppendWritesAll", "state"); err != nil {
		return nil, err
	}
	n, _ := props["immutabilityPeriodSinceCreationInDays"].(json.Number)
	days, perr := n.Int64()
	if perr != nil || days < 1 || days > 146000 {
		return nil, invalidParameter("properties.immutabilityPeriodSinceCreationInDays",
			"The immutability period must be between 1 and 146000 days.")
	}
	appendWrites, _ := boolean(props, "allowProtectedAppendWrites")
	appendWritesAll, _ := boolean(props, "allowProtectedAppendWritesAll")
	s.mu.Lock()
	defer s.mu.Unlock()
	res, aerr := s.wormContainer(c)
	if aerr != nil {
		return nil, aerr
	}
	p := res.worm.policy
	if m := c.r.Header.Get("If-Match"); m != "" && (p == nil || m != "*" && m != p.etag) {
		return nil, preconditionFailed()
	}
	if res.worm.locked() {
		return nil, policyLocked()
	}
	update := "put"
	if p == nil {
		p = &retentionPolicy{}
		res.worm.policy = p
	}
	p.days, p.appendWrites, p.appendWritesAll = days, appendWrites, appendWritesAll
	s.touchPolicy(p, update)
	return s.policyReply(res), nil
}

func (s *Server) lockImmutabilityPolicy(c *call) (*reply, *apiError) {
	m := c.r.Header.Get("If-Match")
	if m == "" {
		return nil, badRequest("The If-Match header is required to lock an immutability policy.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, aerr := s.wormContainer(c)
	if aerr != nil {
		return nil, aerr
	}
	p := res.worm.policy
	if p == nil || m != "*" && m != p.etag {
		return nil, preconditionFailed()
	}
	p.locked = true
	s.touchPolicy(p, "lock")
	return s.policyReply(res), nil
}

// touchPolicy gives p a new ETag and records the change in its history. The caller holds s.mu.
func (s *Server) touchPolicy(p *retentionPolicy, update string) {
	p.etag = quotedETag(s.newUUID())
	p.history = append(p.history, map[string]any{
		"update": update, "immutabilityPeriodSinceCreationInDays": p.days, "timestamp": timestamp(s.now()),
		"objectIdentifier": clientID, "tenantId": clientID,
	})
}

func (s *Server) policyReply(res *resource) *reply {
	rep := ok200(map[string]any{
		"id": res.id + "/immutabilityPolicies/default", "name": "default", "type": immutabilityPolicyType,
		"etag": res.worm.policy.etag, "properties": policyProperties(res.worm.policy),
	})
	rep.header = http.Header{"Etag": {res.worm.policy.etag}}
	return rep
}

func policyProperties(p *retentionPolicy) map[string]any {
	props := map[string]any{"immutabilityPeriodSinceCreationInDays": p.days, "state": "Unlocked"}
	if p.locked {
		props["state"] = "Locked"
	}
	if p.appendWrites {
		props["allowProtectedAppendWrites"] = true
	}
	if p.appendWritesAll {
		props["allowProtectedAppendWritesAll"] = true
	}
	return props
}

// renderWorm adds a container's legal hold and immutability policy to its properties.
func renderWorm(res *resource, props map[string]any) {
	w := res.worm
	props["hasLegalHold"] = w.held()
	props["hasImmutabilityPolicy"] = w != nil && w.policy != nil
	if w.held() {
		tags := make([]any, 0, len(w.tags))
		for _, t := range w.tags {
			tags = append(tags, map[string]any{"tag": t.tag, "timestamp": timestamp(t.at), "objectIdentifier": clientID, "tenantId": clientID})
		}
		props["legalHold"] = map[string]any{"hasLegalHold": true, "tags": tags}
	}
	if w != nil && w.policy != nil {
		props["immutabilityPolicy"] = map[string]any{
			"etag": w.policy.etag, "properties": policyProperties(w.policy), "updateHistory": w.policy.history,
		}
	}
}

func policyLocked() *apiError {
	return newError(http.StatusConflict, "ContainerImmutabilityPolicyLocked",
		"The requested operation is not allowed as the container has a locked immutability policy.")
}

func legalHoldPresent() *apiError {
	return newError(http.StatusConflict, "ContainerHasLegalHold", "The requested operation is not allowed as the container has one or more legal holds.")
}

// blobImmutable fails a delete or overwrite of a blob made at created that
// the container res keeps immutable at now, or returns nil.
func blobImmutable(res *resource, created, now time.Time) *apiError {
	switch {
	case res.worm.held():
		return newError(http.StatusConflict, "BlobImmutableDueToLegalHold",
			"This operation is not permitted as the blob is immutable due to one or more legal holds.")
	case res.worm.protects(created, now):
		return newError(http.StatusConflict, "BlobImmutableDueToPolicy", "This operation is not permitted as the blob is immutable due to a policy.")
	}
	return nil
}

// containerProtected fails a deletion of the container res, or returns nil.
// Through ARM, a container under a legal hold or a policy can be deleted once
// it is empty; through the Blob service, not at all.
// The caller holds s.mu.
func (s *Server) containerProtected(res *resource, dataPlane bool) *apiError {
	if !dataPlane && !s.holdsBlobs(res) {
		return nil
	}
	switch {
	case res.worm.held():
		return legalHoldPresent()
	case res.worm.locked():
		return policyLocked()
	case res.worm != nil && res.worm.policy != nil:
		return newError(http.StatusConflict, "BlobImmutableDueToPolicy", "This operation is not permitted as the blob is immutable due to a policy.")
	}
	return nil
}

// accountProtected fails a deletion of the storage account acct that a
// container's locked policy, or legal hold on blobs it holds, protects, or
// returns nil. The caller holds s.mu.
func (s *Server) accountProtected(acct *resource) *apiError {
	var locked, held []string
	prefix := key(acct.id) + "/"
	for k, res := range s.resources {
		if res.kind != containerKind || !strings.HasPrefix(k, prefix) {
			continue
		}
		switch {
		case res.worm.locked():
			locked = append(locked, res.name)
		case res.worm.held() && s.holdsBlobs(res):
			held = append(held, res.name)
		}
	}
	slices.Sort(locked)
	slices.Sort(held)
	switch {
	case len(locked) > 0:
		return newError(http.StatusConflict, "AccountProtectedFromDeletion",
			"%s is protected from deletion.The container(s) %s have a locked immutability policy.", acct.name, strings.Join(locked, ", "))
	case len(held) > 0:
		return newError(http.StatusConflict, "AccountProtectedFromDeletion",
			"%s is protected from deletion.The container(s) %s have a legal hold.", acct.name, strings.Join(held, ", "))
	}
	return nil
}
