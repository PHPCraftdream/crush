package env

import (
	"os"
	"strings"
)

type Env interface {
	Get(key string) string
	Env() []string
}

type osEnv struct{}

// Get implements Env.
func (o *osEnv) Get(key string) string {
	return os.Getenv(key)
}

func (o *osEnv) Env() []string {
	return os.Environ()
}

func New() Env {
	return &osEnv{}
}

// overlayEnv wraps a base Env and shadows a fixed set of keys with values
// supplied explicitly at construction time, without ever touching the
// process's real environment (os.Setenv/os.Unsetenv). Overlay keys take
// priority over the base for both Get and Env(); every other key passes
// through to base unchanged.
//
// This exists so a caller-scoped override (e.g. "CRUSH_"-prefixed config
// values that should be visible under their bare name to shell expansion)
// can be threaded explicitly through a single Env value — and from there
// into anything that reads it (config.Resolve, os/exec child processes via
// the env []string each of those already accepts) — instead of mutating
// global process state that every concurrent goroutine/reload/child
// process would also observe. See PushPopCrushEnv's replacement in
// internal/config/load.go for the motivating case.
type overlayEnv struct {
	base    Env
	overlay map[string]string // already stripped of any key prefix
}

// NewOverlay returns an Env that reads from base, except for any key
// present in overlay, which wins. A nil base is treated as an empty Env.
func NewOverlay(base Env, overlay map[string]string) Env {
	if base == nil {
		base = NewFromMap(nil)
	}
	return &overlayEnv{base: base, overlay: overlay}
}

// Get implements Env.
func (o *overlayEnv) Get(key string) string {
	if v, ok := o.overlay[key]; ok {
		return v
	}
	return o.base.Get(key)
}

// Env implements Env. Overlay entries are appended after the base
// environment so a shell's "last assignment wins" lookup semantics (and
// expand.ListEnviron, which scans in order) resolve to the overlay value
// for any shadowed key.
func (o *overlayEnv) Env() []string {
	baseEnv := o.base.Env()
	if len(o.overlay) == 0 {
		return baseEnv
	}
	out := make([]string, 0, len(baseEnv)+len(o.overlay))
	for _, kv := range baseEnv {
		key, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, shadowed := o.overlay[key]; shadowed {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range o.overlay {
		out = append(out, k+"="+v)
	}
	return out
}

type mapEnv struct {
	m map[string]string
}

// Get implements Env.
func (m *mapEnv) Get(key string) string {
	if value, ok := m.m[key]; ok {
		return value
	}
	return ""
}

// Env implements Env.
func (m *mapEnv) Env() []string {
	env := make([]string, 0, len(m.m))
	for k, v := range m.m {
		env = append(env, k+"="+v)
	}
	return env
}

func NewFromMap(m map[string]string) Env {
	if m == nil {
		m = make(map[string]string)
	}
	return &mapEnv{m: m}
}
