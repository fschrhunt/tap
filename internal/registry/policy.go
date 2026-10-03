package registry

import (
	"fmt"
	"path"
	"strings"

	"github.com/fschrhunt/tap/internal/wire"
)

// Failure classifies gateway failures without guessing whether a downstream write succeeded.
type Failure struct{ Code, Message, Recovery string }

// Error supplies a concise CLI diagnostic; structured MCP errors retain the code and recovery.
func (f *Failure) Error() string { return f.Message }

// causedFailure preserves SDK cancellation identity alongside value-free gateway diagnostics.
type causedFailure struct {
	failure *Failure
	cause   error
}

// Error exposes only the safe gateway message, never the underlying transport error.
func (f *causedFailure) Error() string { return f.failure.Error() }

// Unwrap supports both machine-readable gateway recovery and errors.Is for cancellation.
func (f *causedFailure) Unwrap() []error { return []error{f.failure, f.cause} }

// checkPolicy matches path globs only on slash-free names, rejecting unsupported names
// before any connection. Backend annotations never grant access.
func checkPolicy(def wire.Object, tool string) error {
	if strings.Contains(tool, "/") {
		return &Failure{"permission_denied", "tool names containing / are unsupported", "Use a downstream tool with a slash-free name."}
	}
	p, err := policy(def)
	if err != nil {
		return err
	}
	for _, key := range []string{"deny", "allow"} {
		patterns, present := p.Get(key).([]any)
		if !present {
			continue
		}
		matched := false
		for _, raw := range patterns {
			pattern := raw.(string)
			hit, _ := path.Match(pattern, tool)
			matched = matched || hit
		}
		if key == "deny" && matched || key == "allow" && !matched {
			return &Failure{"permission_denied", "tool is blocked by the configured policy", "Ask the user to change policy outside the agent session; do not retry unchanged."}
		}
	}
	return nil
}

// policy fails closed on malformed rules rather than silently broadening access.
func policy(def wire.Object) (wire.Object, error) {
	if !def.Has("policy") {
		return wire.Object{}, nil
	}
	p, ok := def.Get("policy").(wire.Object)
	if !ok {
		return nil, &Failure{"invalid_policy", "policy must be an object", "Fix the server configuration."}
	}
	for _, f := range p {
		if f.Name != "allow" && f.Name != "deny" && f.Name != "referenceTo" {
			return nil, &Failure{"invalid_policy", "unknown policy field", "Use allow, deny and referenceTo only."}
		}
		a, ok := f.Value.([]any)
		if !ok {
			return nil, &Failure{"invalid_policy", "policy rules must be arrays of strings", "Fix the server configuration."}
		}
		for _, v := range a {
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, &Failure{"invalid_policy", "policy rules must contain nonempty strings", "Fix the server configuration."}
			}
			if f.Name != "referenceTo" {
				if _, err := path.Match(s, ""); err != nil {
					return nil, &Failure{"invalid_policy", fmt.Sprintf("invalid %s pattern", f.Name), "Fix the server configuration."}
				}
			}
		}
	}
	return p, nil
}

// checkReferenceFlow requires an explicit source-side grant for cross-server data transfer.
func checkReferenceFlow(def wire.Object, source, target string) error {
	p, err := policy(def)
	if err != nil {
		return err
	}
	if source == target {
		return nil
	}
	if allowed, ok := p.Get("referenceTo").([]any); ok {
		for _, v := range allowed {
			if v == target {
				return nil
			}
		}
	}
	return &Failure{"reference_flow_denied", "cross-server result transfer is not permitted", "The user must grant the destination server in the source server's policy.referenceTo."}
}
