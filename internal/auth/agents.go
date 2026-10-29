package auth

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A delegate's agents are limited by the operator, provider by provider
// and, for each, model by model. The limits name providers and models
// whether or not this machine has them now: a provider turned on later
// is already limited when it comes. nil is no limit (every agent the
// house runs); an empty map is no agent at all. A provider listed with
// no models may use every model of that provider.

// ErrAgentNotAllowed is a delegate asking for an agent or a model their
// limits leave out.
var ErrAgentNotAllowed = errors.New("not one of the agents your account may use")

var providerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CheckAgents normalizes limits: providers by their key, models trimmed,
// unique and sorted. nil stays nil.
func CheckAgents(limits map[string][]string) (map[string][]string, error) {
	if limits == nil {
		return nil, nil
	}
	if len(limits) > 64 {
		return nil, fmt.Errorf("too many providers")
	}
	out := make(map[string][]string, len(limits))
	for p, models := range limits {
		p = strings.ToLower(strings.TrimSpace(p))
		if !providerRE.MatchString(p) {
			return nil, fmt.Errorf("%q is not a provider", p)
		}
		if len(models) > 128 {
			return nil, fmt.Errorf("%s: too many models", p)
		}
		list := []string{}
		for _, m := range models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if len(m) > 128 || strings.ContainsAny(m, " \t\r\n") {
				return nil, fmt.Errorf("%s: %q is not a model", p, m)
			}
			if !slices.Contains(list, m) {
				list = append(list, m)
			}
		}
		sort.Strings(list)
		out[p] = list
	}
	return out, nil
}

// AgentAllowed says whether limits let provider run model; model "" asks
// about the provider alone.
func AgentAllowed(limits map[string][]string, provider, model string) bool {
	if limits == nil {
		return true
	}
	models, ok := limits[provider]
	if !ok {
		return false
	}
	return len(models) == 0 || model == "" || slices.Contains(models, model)
}

// PickAgentModel is the model a delegate's turn runs with: model when
// limits allow it, and for no model asked (the provider's default) the
// first allowed one the provider knows, so a limited user never lands on
// a default that is not theirs. known is the provider's models here.
func PickAgentModel(limits map[string][]string, provider, model string, known []string) (string, error) {
	if limits == nil {
		return model, nil
	}
	models, ok := limits[provider]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrAgentNotAllowed, provider)
	}
	if len(models) == 0 {
		return model, nil
	}
	if model != "" {
		if !slices.Contains(models, model) {
			return "", fmt.Errorf("%w: %s with %s", ErrAgentNotAllowed, provider, model)
		}
		return model, nil
	}
	// In the provider's order: its first is its default, so a limit that
	// keeps the default lands on it.
	for _, m := range known {
		if slices.Contains(models, m) {
			return m, nil
		}
	}
	return models[0], nil
}
