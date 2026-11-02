package rep

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"code.cloudfoundry.org/bbs/models"
)

// DefaultExecutionBackend is the backend that every cell offers. A workload
// that does not set CF_EXECUTION_BACKEND runs on it.
const DefaultExecutionBackend = "oci-runc"

// ExecutionBackendTagPrefix starts every placement tag that a cell uses to
// advertise its execution backend policy. The cell presence and the cell state
// carry these tags among the optional placement tags, because the BBS models
// have no field for them. Operator placement tags, and so isolation segment
// names, must not start with this prefix.
const ExecutionBackendTagPrefix = "execution-backend."

const (
	offeredBackendTagPrefix   = ExecutionBackendTagPrefix + "offered:"
	requiredBackendTagPrefix  = ExecutionBackendTagPrefix + "required:"
	exemptTaskDomainTagPrefix = ExecutionBackendTagPrefix + "exempt-task-domain:"
)

var (
	// ErrExecutionBackendNotOffered is returned when a workload selects an
	// execution backend that the cell does not offer.
	ErrExecutionBackendNotOffered = errors.New("execution backend is not offered by this cell")

	// ErrExecutionBackendNotAccepted is returned when the cell offers the
	// selected backend but requires its workloads to select another one.
	ErrExecutionBackendNotAccepted = errors.New("execution backend is not accepted by this cell")
)

// ExecutionBackendPolicy describes which execution backends a cell offers and
// which of them it requires workloads to select.
//
// Offered names the backends that the cell offers in addition to oci-runc,
// which every cell offers. When Required is not empty, the cell accepts only
// workloads that select one of the backends in it, except tasks whose domain
// is in RequiredExemptTaskDomains, which accept any offered backend. Cloud
// Controller stages apps in tasks that select no backend, so the staging
// domain is the usual exemption.
type ExecutionBackendPolicy struct {
	offered                   map[string]struct{}
	required                  map[string]struct{}
	requiredExemptTaskDomains map[string]struct{}
}

// NewExecutionBackendPolicy builds a policy from the cell's configuration. It
// refuses a required backend that the cell does not offer, because no
// workload could then satisfy it.
func NewExecutionBackendPolicy(offered, required, requiredExemptTaskDomains []string) (ExecutionBackendPolicy, error) {
	policy := ExecutionBackendPolicy{
		offered:                   toNameSet(offered),
		required:                  toNameSet(required),
		requiredExemptTaskDomains: toNameSet(requiredExemptTaskDomains),
	}
	delete(policy.offered, DefaultExecutionBackend)

	for name := range policy.offered {
		if name == "" || strings.ContainsAny(name, ", ") {
			return ExecutionBackendPolicy{}, fmt.Errorf("invalid offered execution backend %q", name)
		}
	}
	for name := range policy.required {
		if !policy.offers(name) {
			return ExecutionBackendPolicy{}, fmt.Errorf("required execution backend %q is not offered by this cell", name)
		}
	}
	for domain := range policy.requiredExemptTaskDomains {
		if domain == "" {
			return ExecutionBackendPolicy{}, errors.New("an exempt task domain is empty")
		}
	}
	if len(policy.required) == 0 {
		policy.requiredExemptTaskDomains = map[string]struct{}{}
	}

	return policy, nil
}

// ExecutionBackendPolicyFromPlacementTags reads the policy that a cell
// advertises in its placement tags, ignoring every other tag. A cell that
// advertises nothing, which includes every cell whose rep predates the
// policy, offers only oci-runc and requires nothing.
func ExecutionBackendPolicyFromPlacementTags(tags []string) ExecutionBackendPolicy {
	policy := ExecutionBackendPolicy{
		offered:                   map[string]struct{}{},
		required:                  map[string]struct{}{},
		requiredExemptTaskDomains: map[string]struct{}{},
	}
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, offeredBackendTagPrefix):
			policy.offered[strings.TrimPrefix(tag, offeredBackendTagPrefix)] = struct{}{}
		case strings.HasPrefix(tag, requiredBackendTagPrefix):
			policy.required[strings.TrimPrefix(tag, requiredBackendTagPrefix)] = struct{}{}
		case strings.HasPrefix(tag, exemptTaskDomainTagPrefix):
			policy.requiredExemptTaskDomains[strings.TrimPrefix(tag, exemptTaskDomainTagPrefix)] = struct{}{}
		}
	}
	return policy
}

// PlacementTags returns the tags that advertise the policy, sorted so that the
// cell presence does not change between rep restarts.
func (p ExecutionBackendPolicy) PlacementTags() []string {
	tags := []string{}
	for _, name := range sortedNames(p.offered) {
		tags = append(tags, offeredBackendTagPrefix+name)
	}
	for _, name := range sortedNames(p.required) {
		tags = append(tags, requiredBackendTagPrefix+name)
	}
	for _, domain := range sortedNames(p.requiredExemptTaskDomains) {
		tags = append(tags, exemptTaskDomainTagPrefix+domain)
	}
	return tags
}

// Advertised reports whether the policy offers a backend beyond oci-runc or
// requires one, which is when placement needs to know what workloads select.
func (p ExecutionBackendPolicy) Advertised() bool {
	return len(p.offered) > 0 || len(p.required) > 0
}

// Required returns the backends that the cell requires, sorted, or nil when
// it requires none.
func (p ExecutionBackendPolicy) Required() []string {
	if len(p.required) == 0 {
		return nil
	}
	return sortedNames(p.required)
}

// Check returns nil when the cell can run a workload that selects backend.
// taskDomain is the domain of a task and empty for an LRP. An empty backend
// is oci-runc. The error wraps ErrExecutionBackendNotOffered or
// ErrExecutionBackendNotAccepted and names the backend.
func (p ExecutionBackendPolicy) Check(backend, taskDomain string) error {
	name := backend
	if name == "" {
		name = DefaultExecutionBackend
	}

	if !p.offers(name) {
		return fmt.Errorf("%w: %q", ErrExecutionBackendNotOffered, name)
	}

	if len(p.required) == 0 {
		return nil
	}
	if _, ok := p.required[name]; ok {
		return nil
	}
	if taskDomain != "" {
		if _, ok := p.requiredExemptTaskDomains[taskDomain]; ok {
			return nil
		}
	}
	return fmt.Errorf("%w: %q, this cell requires %s", ErrExecutionBackendNotAccepted, name, QuoteExecutionBackends(p.Required()))
}

func (p ExecutionBackendPolicy) offers(name string) bool {
	if name == DefaultExecutionBackend {
		return true
	}
	_, ok := p.offered[name]
	return ok
}

// IsExecutionBackendPolicyError reports whether err is a refusal by Check.
func IsExecutionBackendPolicyError(err error) bool {
	return errors.Is(err, ErrExecutionBackendNotOffered) || errors.Is(err, ErrExecutionBackendNotAccepted)
}

// IsExecutionBackendPlacementTag reports whether tag uses the prefix that is
// reserved for the execution backend policy.
func IsExecutionBackendPlacementTag(tag string) bool {
	return strings.HasPrefix(tag, ExecutionBackendTagPrefix)
}

// QuoteExecutionBackends names a list of backends for a message, such as
// `"vm-qemu"` or `"vm-firecracker" or "vm-qemu"`.
func QuoteExecutionBackends(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, " or ")
}

// ExecutionBackendForDesiredLRP returns the execution backend that an LRP
// selects, or "" when it selects none. The rep passes the same value to
// Garden, so placement and the cell agree on what the LRP runs on.
func ExecutionBackendForDesiredLRP(desiredLRP *models.DesiredLRP) string {
	backend, _ := executionBackendFromLRP(desiredLRP)
	return backend
}

// ExecutionBackendForTask returns the execution backend that a task selects,
// or "" when it selects none.
func ExecutionBackendForTask(task *models.Task) string {
	if task == nil || task.TaskDefinition == nil {
		return ""
	}
	return executionBackendFromEnv(task.EnvironmentVariables)
}

func toNameSet(names []string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
