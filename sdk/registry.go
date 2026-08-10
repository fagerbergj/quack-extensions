package sdk

import "fmt"

var registry = map[string]Factory{}

// Register is called from an extension package's init(); quack's registry
// file blank-imports extension packages to populate this. A duplicate name
// is a programmer error (two extensions claiming the same identity), so it
// panics at init time rather than surfacing as a runtime startup error.
func Register(name string, f Factory) {
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("sdk: extension %q already registered", name))
	}
	registry[name] = f
}

// Registered returns a snapshot of all registered factories, keyed by name.
// It copies the live map so callers can't mutate registration state.
func Registered() map[string]Factory {
	out := make(map[string]Factory, len(registry))
	for name, f := range registry {
		out[name] = f
	}
	return out
}
