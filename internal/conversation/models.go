package conversation

import (
	"context"
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/store"
)

// Models is the model serving each role, and what else could serve it.
type Models struct {
	Roles []RoleModels
}

// RoleModels is one role of the models menu.
type RoleModels struct {
	Role config.Role
	// Current is the model the role uses: the one saved when there is one, and
	// the file's default otherwise.
	Current string
	Saved   bool
	Default string
	Options []string
	// Problem is why the options may be short: a runner that could not be
	// asked what its models can do.
	Problem string
}

// SavedModel is the model a role was given, and false when the configuration
// file's default still stands.
func SavedModel(ctx context.Context, s *store.Store, role config.Role) (string, bool, error) {
	return s.Get(ctx, store.KeyModel(string(role)))
}

// RoleModel is the model that serves a role in this conversation: the one it
// was given, and the file's default while it was given none. It answers nil
// for a role nothing serves, which is not by itself a failure: whoever asks
// says what it means for them.
func RoleModel(ctx context.Context, s *store.Store, set *runners.Setup, role config.Role) (*runners.Configured, error) {
	if set == nil {
		return nil, nil
	}
	name, saved, err := SavedModel(ctx, s, role)
	if err != nil {
		return nil, err
	}
	if saved {
		// A name that is no longer in the file is not a model any more, and
		// what the file says now stands in its place.
		if m := set.Model(name); m != nil {
			return m, nil
		}
	}
	return set.Defaults[role], nil
}

// SavedModels is the model saved for every role, for a command that reads the
// conversation without opening it.
func SavedModels(ctx context.Context, s *store.Store) (map[config.Role]string, error) {
	out := map[config.Role]string{}
	for _, role := range config.Roles {
		name, saved, err := SavedModel(ctx, s, role)
		if err != nil {
			return nil, err
		}
		if saved {
			out[role] = name
		}
	}
	return out, nil
}

// Models says what each role is served by.
func (e *Engine) Models(ctx context.Context) (Models, error) {
	var out Models
	if e.runners == nil {
		return out, nil
	}
	// Every catalogue is read once for the whole menu, rather than once per
	// role, so a runner that is away is asked once.
	serving, problem := e.runners.ServingRoles(ctx, config.Roles)
	for _, role := range config.Roles {
		r := RoleModels{Role: role}
		if problem != nil {
			// A runner that could not be asked leaves its models out, which is
			// said rather than read as models that cannot serve the role.
			r.Problem = problem.Error()
		}
		if m := e.runners.Defaults[role]; m != nil {
			r.Default = m.Name
			r.Current = m.Name
		}
		name, saved, err := SavedModel(ctx, e.store, role)
		if err != nil {
			return out, err
		}
		if saved {
			if m := e.runners.Model(name); m != nil {
				r.Current = name
				r.Saved = true
			}
		}
		for _, m := range serving[role] {
			r.Options = append(r.Options, m.Name)
		}
		if r.Current == "" && len(r.Options) == 0 && r.Problem == "" {
			continue
		}
		out.Roles = append(out.Roles, r)
	}
	return out, nil
}

// SetModel remembers which model serves a role.
func (e *Engine) SetModel(ctx context.Context, role config.Role, name string) error {
	if !slices.Contains(config.Roles, role) {
		return fmt.Errorf("there is no %q to set, only %v", role, config.Roles)
	}
	if e.runners == nil || e.runners.Model(name) == nil {
		return fmt.Errorf("no model is called %q", name)
	}
	serving, problem := e.runners.Serving(ctx, role)
	if !slices.ContainsFunc(serving, func(m *runners.Configured) bool { return m.Name == name }) {
		if problem != nil {
			return fmt.Errorf("%s cannot be held against the %s role: %w", name, role, problem)
		}
		return fmt.Errorf("%s cannot be the %s model", name, role)
	}
	return e.store.Set(ctx, store.KeyModel(string(role)), name)
}

// ResetModels forgets every saved choice, so the file decides again.
func (e *Engine) ResetModels(ctx context.Context) error {
	for _, role := range config.Roles {
		if err := e.store.Delete(ctx, store.KeyModel(string(role))); err != nil {
			return err
		}
	}
	return nil
}
