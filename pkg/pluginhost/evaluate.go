package pluginhost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

type producerKey struct {
	Plugin  string
	Trigger string
	ID      string
}

// Kubernetes API versions are representations of the same concrete object.
// Keep this collision identity separate from versioned wire/trigger identities.
type concreteIdentity struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

type output struct {
	Key       producerKey
	Resources []plugin.Resource
}

type state struct {
	Outputs     []output
	Diagnostics []plugin.Diagnostic
}

func (h *Host) evaluate(ctx context.Context, sessions []string, result *Result) (*Result, error) {
	previous := state{Outputs: []output{}, Diagnostics: []plugin.Diagnostic{}}
	previousKey, err := stateKey(previous)
	if err != nil {
		return result, err
	}
	// The empty seed is not an accepted engine response. A source may disappear
	// after activation, so returning to that seed first needs a confirming sweep.
	seen := map[[sha256.Size]byte]bool{}
	for range h.options.MaxIterations {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		inventory := resources(previous)
		responses := make([]*plugin.ExpandResponse, len(h.engines))
		for i, e := range h.engines {
			response, err := e.service.Expand(ctx, &plugin.ExpandRequest{
				Session: sessions[i], Resources: slices.Clone(inventory),
			})
			if err != nil {
				return result, fmt.Errorf("plugin %q expand: %w", e.command.Name, err)
			}
			if response == nil {
				return result, fmt.Errorf("plugin %q returned nil expansion response", e.command.Name)
			}
			// Snapshot responses even for in-process services reusing backing arrays.
			data, err := json.Marshal(response)
			if err != nil {
				return result, fmt.Errorf("plugin %q response: %w", e.command.Name, err)
			}
			responses[i] = &plugin.ExpandResponse{}
			if err := json.Unmarshal(data, responses[i]); err != nil {
				return result, err
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		next, evidence, err := h.replace(previous, responses)
		if err != nil {
			return result, err
		}
		key, err := stateKey(next)
		if err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Diagnostics, result.Evidence = next.Diagnostics, evidence
		if key == previousKey {
			result.Complete = true
			for _, diagnostic := range next.Diagnostics {
				if strings.EqualFold(diagnostic.Severity, "error") {
					result.Complete = false
				}
			}
			if result.Complete {
				result.Resources = resources(next)
			}
			return result, nil
		}
		if seen[key] {
			result.Diagnostics = append(result.Diagnostics, plugin.Diagnostic{
				Code: "oscillation", Severity: "error", Message: "plugin desired inventory repeated a non-stable state",
			})
			return result, errors.New("plugin rendering oscillated")
		}
		seen[key] = true
		previous, previousKey = next, key
	}
	result.Diagnostics = append(result.Diagnostics, plugin.Diagnostic{
		Code: "iteration-limit", Severity: "error", Message: "plugin rendering did not converge within the sweep limit",
	})
	return result, errors.New("plugin rendering exceeded iteration limit")
}

// replace validates the entire sweep before accepting any outputs. Ownership is
// replaced exhaustively, then activation reachability is recomputed from roots.
// In particular, a disconnected mutually-triggering SCC cannot keep itself alive.
func (h *Host) replace(previous state, responses []*plugin.ExpandResponse) (state, []json.RawMessage, error) {
	next := state{Outputs: []output{}, Diagnostics: []plugin.Diagnostic{}}
	evidence := []json.RawMessage{}
	candidateIDs := map[string]bool{}
	keys := map[producerKey]bool{}
	for i, response := range responses {
		name := h.engines[i].command.Name
		for _, expansion := range response.Expansions {
			key := producerKey{Plugin: name, Trigger: expansion.Trigger, ID: expansion.ID}
			emptyIdentity := expansion.ID == "" || expansion.Trigger == ""
			if emptyIdentity || keys[key] {
				return next, nil, fmt.Errorf("plugin %q empty or duplicate expansion identity %+v", name, key)
			}
			if expansion.Trigger == "root" && !h.roots[name] {
				return next, nil, fmt.Errorf("plugin %q is not approved to produce roots", name)
			}
			keys[key] = true
			for _, resource := range expansion.Resources {
				if err := validateResource(resource); err != nil {
					return next, nil, fmt.Errorf("plugin %q expansion %q: %w", name, expansion.ID, err)
				}
				candidateIDs[resource.ID] = true
			}
			// Duplicates within one producer's batch are malformed regardless of
			// activation. Distinct producers may temporarily claim the same ID;
			// their ownership is checked only after activation pruning.
			batch := output{Key: key, Resources: expansion.Resources}
			if err := validateOwnership([]output{batch}); err != nil {
				return next, nil, err
			}
			slices.SortFunc(expansion.Resources, func(a, b plugin.Resource) int { return strings.Compare(a.ID, b.ID) })
			next.Outputs = append(next.Outputs, output{Key: key, Resources: expansion.Resources})
		}
		next.Diagnostics = append(next.Diagnostics, response.Diagnostics...)
		evidence = append(evidence, response.Evidence...)
	}
	oldIDs := map[string]bool{}
	for _, resource := range resources(previous) {
		oldIDs[resource.ID] = true
	}
	for _, expansion := range next.Outputs {
		trigger := expansion.Key.Trigger
		exists := candidateIDs[trigger]
		missing := trigger != "root" && !exists
		if missing && !oldIDs[trigger] {
			next.Diagnostics = append(next.Diagnostics, plugin.Diagnostic{
				Code: "missing-trigger", Severity: "error", ResourceID: trigger,
				Message: fmt.Sprintf("plugin %q expansion %q references absent trigger %q",
					expansion.Key.Plugin, expansion.Key.ID, trigger),
			})
		}
	}
	reachable := map[string]bool{}
	active := make([]bool, len(next.Outputs))
	for changed := true; changed; {
		changed = false
		for i, expansion := range next.Outputs {
			if active[i] {
				continue
			}
			if expansion.Key.Trigger != "root" && !reachable[expansion.Key.Trigger] {
				continue
			}
			active[i], changed = true, true
			for _, resource := range expansion.Resources {
				reachable[resource.ID] = true
			}
		}
	}
	retained := make([]output, 0, len(next.Outputs))
	for i, expansion := range next.Outputs {
		if active[i] {
			retained = append(retained, expansion)
		}
	}
	next.Outputs = retained
	if err := validateOwnership(retained); err != nil {
		return next, nil, err
	}
	slices.SortFunc(next.Outputs, func(a, b output) int {
		if c := strings.Compare(a.Key.Plugin, b.Key.Plugin); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key.Trigger, b.Key.Trigger); c != 0 {
			return c
		}
		return strings.Compare(a.Key.ID, b.Key.ID)
	})
	// Diagnostics are exhaustive replacements, including pending errors. Never
	// erase an engine error merely because its resource was pruned or not produced.
	slices.SortFunc(next.Diagnostics, func(a, b plugin.Diagnostic) int {
		aJSON, _ := json.Marshal(a)
		bJSON, _ := json.Marshal(b)
		return strings.Compare(string(aJSON), string(bJSON))
	})
	return next, evidence, nil
}

func validateOwnership(outputs []output) error {
	owners := map[string]producerKey{}
	concreteOwners := map[concreteIdentity]producerKey{}
	for _, expansion := range outputs {
		for _, resource := range expansion.Resources {
			if owner, exists := owners[resource.ID]; exists {
				return fmt.Errorf("duplicate resource identity %q from %+v and %+v", resource.ID, owner, expansion.Key)
			}
			owners[resource.ID] = expansion.Key
			if resource.Logical {
				continue
			}
			identity, err := concreteKey(resource)
			if err != nil {
				return err
			}
			if owner, exists := concreteOwners[identity]; exists {
				return fmt.Errorf("duplicate concrete resource identity %+v from %+v and %+v",
					identity, owner, expansion.Key)
			}
			concreteOwners[identity] = expansion.Key
		}
	}
	return nil
}

func validateResource(resource plugin.Resource) error {
	if resource.ID == "" || resource.ID == "root" {
		return errors.New("empty or reserved resource identity")
	}
	object, err := plugin.Object(resource)
	if err != nil {
		return fmt.Errorf("resource %q YAML: %w", resource.ID, err)
	}
	if _, err := json.Marshal(object); err != nil {
		return fmt.Errorf("resource %q JSON: %w", resource.ID, err)
	}
	metadata := map[string]any{}
	if raw, exists := object["metadata"]; exists {
		var ok bool
		metadata, ok = raw.(map[string]any)
		if !ok {
			return fmt.Errorf("resource %q metadata must be an object", resource.ID)
		}
	}
	for _, key := range []string{"name", "namespace", "generateName"} {
		if raw, exists := metadata[key]; exists {
			if _, ok := raw.(string); !ok {
				return fmt.Errorf("resource %q metadata.%s must be a string", resource.ID, key)
			}
		}
	}
	name, _ := metadata["name"].(string)
	if name == "" && resource.Logical {
		if !strings.HasPrefix(resource.ID, "logical:") || resource.ID == "logical:" {
			return fmt.Errorf("resource %q must have a logical: identity", resource.ID)
		}
		apiVersion, _ := object["apiVersion"].(string)
		kind, _ := object["kind"].(string)
		if apiVersion == "" || kind == "" {
			return fmt.Errorf("logical resource %q requires apiVersion and kind", resource.ID)
		}
		return nil
	}
	id, err := plugin.ObjectID(object)
	if err != nil {
		return fmt.Errorf("resource %q identity: %w", resource.ID, err)
	}
	if id != resource.ID || resource.Logical {
		return fmt.Errorf("resource identity %q disagrees with named object identity %q", resource.ID, id)
	}
	return nil
}

func resources(s state) []plugin.Resource {
	result := []plugin.Resource{}
	for _, expansion := range s.Outputs {
		result = append(result, expansion.Resources...)
	}
	slices.SortFunc(result, func(a, b plugin.Resource) int { return strings.Compare(a.ID, b.ID) })
	return result
}

func concreteKey(resource plugin.Resource) (concreteIdentity, error) {
	object, err := plugin.Object(resource)
	if err != nil {
		return concreteIdentity{}, err
	}
	apiVersion, _ := object["apiVersion"].(string)
	group, _, grouped := strings.Cut(apiVersion, "/")
	if !grouped {
		group = ""
	}
	metadata, _ := object["metadata"].(map[string]any)
	kind, _ := object["kind"].(string)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	return concreteIdentity{Group: group, Kind: kind, Namespace: namespace, Name: name}, nil
}

func stateKey(s state) ([sha256.Size]byte, error) {
	// Canonical JSON compares parsed YAML semantics, preserving all actual
	// metadata, rather than byte formatting or inventory cardinality.
	canonical := state{Outputs: make([]output, 0, len(s.Outputs)), Diagnostics: s.Diagnostics}
	for _, expansion := range s.Outputs {
		canonicalOutput := output{Key: expansion.Key, Resources: make([]plugin.Resource, len(expansion.Resources))}
		copy(canonicalOutput.Resources, expansion.Resources)
		for i := range canonicalOutput.Resources {
			object, err := plugin.Object(canonicalOutput.Resources[i])
			if err != nil {
				return [sha256.Size]byte{}, err
			}
			data, err := json.Marshal(object)
			if err != nil {
				return [sha256.Size]byte{}, err
			}
			canonicalOutput.Resources[i].YAML = string(data)
		}
		canonical.Outputs = append(canonical.Outputs, canonicalOutput)
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	// Remember fingerprints, not a full inventory per sweep, to bound history memory.
	return sha256.Sum256(data), nil
}
