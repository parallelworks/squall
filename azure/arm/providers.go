package arm

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
)

const (
	providerNotRegistered = "NotRegistered"
	providerRegistering   = "Registering"
	providerRegistered    = "Registered"
)

// WithUnregisteredProviders makes the resource provider namespaces start
// NotRegistered in every subscription. Every other namespace starts
// Registered.
func WithUnregisteredProviders(namespaces ...string) Option {
	return func(s *Server) {
		for _, ns := range namespaces {
			s.unregistered[strings.ToLower(ns)] = ns
		}
	}
}

// provider is a resource provider's registration in a subscription.
type provider struct {
	sub, namespace string
	state          string
	pending        *operation
	registrations  int
}

func (s *Server) registerProviderRoutes() {
	s.add("GET", sub+"/providers/{ns}", "Microsoft.Resources/subscriptions/providers/read", false, s.getProvider)
	s.add("POST", sub+"/providers/{ns}/register", "{ns}/register/action", false, s.registerProvider)
}

func providerKey(sub, ns string) string { return key(sub + "/" + ns) }

// provider returns the registration of ns in sub, recording it when new. The caller holds s.mu.
func (s *Server) provider(sub, ns string) *provider {
	k := providerKey(sub, ns)
	if p := s.providers[k]; p != nil {
		return p
	}
	p := &provider{sub: sub, namespace: ns, state: providerRegistered}
	if name, ok := s.unregistered[strings.ToLower(ns)]; ok {
		p.namespace, p.state = name, providerNotRegistered
	}
	s.providers[k] = p
	return p
}

// providerState returns the registration state of ns in sub without recording it. The caller holds s.mu.
func (s *Server) providerState(sub, ns string) string {
	if p := s.providers[providerKey(sub, ns)]; p != nil {
		return p.state
	}
	if _, ok := s.unregistered[strings.ToLower(ns)]; ok {
		return providerNotRegistered
	}
	return providerRegistered
}

// readProvider returns the registration of ns in sub, counting the read toward a registration in progress. The caller holds s.mu.
func (s *Server) readProvider(sub, ns string) *provider {
	p := s.provider(sub, ns)
	s.observe(p.pending)
	return p
}

func renderProvider(p *provider) map[string]any {
	return map[string]any{
		"id": "/subscriptions/" + p.sub + "/providers/" + p.namespace, "namespace": p.namespace,
		"registrationState": p.state, "registrationPolicy": "RegistrationRequired", "resourceTypes": []any{},
	}
}

func (s *Server) getProvider(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ok200(renderProvider(s.readProvider(c.val("sub"), c.val("ns")))), nil
}

// registerProvider starts registering a namespace and answers 200 with it
// Registering, as ARM does, with no operation to poll; the registration ends
// after the reads [WithSettleAfter] sets.
func (s *Server) registerProvider(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.provider(c.val("sub"), c.val("ns"))
	p.registrations++
	if p.state == providerNotRegistered {
		p.state = providerRegistering
		op := s.begin(c, opPaths{}, 0, func() *apiError {
			p.state, p.pending = providerRegistered, nil
			return nil
		})
		if op.status == opInProgress {
			p.pending = op
		}
	}
	return ok200(renderProvider(p)), nil
}

// routeNamespace is the resource provider namespace a route's pattern names
// after its first providers segment, or "" when it names none.
func routeNamespace(rt *route) string {
	i := slices.Index(rt.segments, "providers")
	if i < 0 || i+1 >= len(rt.segments) || strings.HasPrefix(rt.segments[i+1], "{") {
		return ""
	}
	return rt.segments[i+1]
}

// checkRegistration refuses a call to a namespace its subscription has not
// registered, as ARM refuses it. A namespace being registered takes calls.
func (s *Server) checkRegistration(rt *route, vals map[string]string) *apiError {
	ns := routeNamespace(rt)
	if ns == "" || len(s.unregistered) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.providerState(vals["sub"], ns) != providerNotRegistered {
		return nil
	}
	e := newError(http.StatusConflict, "MissingSubscriptionRegistration",
		"The subscription is not registered to use namespace '%s'. See https://aka.ms/rps-not-found for how to register subscriptions.", ns)
	e.Details = []errorDetail{{Code: e.Code, Target: ns, Message: e.Message}}
	return e
}

// ProviderRegistration is a snapshot of a resource provider's registration in a subscription.
type ProviderRegistration struct {
	Subscription string
	Namespace    string
	// State is NotRegistered, Registering or Registered.
	State string
	// Registrations counts the register calls made for it.
	Registrations int
}

// ProviderRegistrations returns the registrations calls have read or
// registered, ordered by subscription and namespace.
func (s *Server) ProviderRegistrations() []ProviderRegistration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProviderRegistration, 0, len(s.providers))
	for _, p := range s.providers {
		out = append(out, ProviderRegistration{Subscription: p.sub, Namespace: p.namespace, State: p.state, Registrations: p.registrations})
	}
	slices.SortFunc(out, func(a, b ProviderRegistration) int {
		return cmp.Or(cmp.Compare(key(a.Subscription), key(b.Subscription)), cmp.Compare(key(a.Namespace), key(b.Namespace)))
	})
	return out
}

// ProviderState returns the registration state of the namespace ns in the subscription sub.
func (s *Server) ProviderState(sub, ns string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerState(sub, ns)
}
