package env

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOsEnv_Get(t *testing.T) {
	env := &osEnv{}

	// Test getting an existing environment variable
	t.Setenv("TEST_VAR", "test_value")

	value := env.Get("TEST_VAR")
	require.Equal(t, "test_value", value)

	// Test getting a non-existent environment variable
	value = env.Get("NON_EXISTENT_VAR")
	require.Equal(t, "", value)
}

func TestOsEnv_Env(t *testing.T) {
	env := &osEnv{}

	envVars := env.Env()

	// Environment should not be empty in normal circumstances
	require.NotNil(t, envVars)
	require.Greater(t, len(envVars), 0)

	// Each environment variable should be in key=value format
	for _, envVar := range envVars {
		require.Contains(t, envVar, "=")
	}
}

func TestNewFromMap(t *testing.T) {
	testMap := map[string]string{
		"KEY1": "value1",
		"KEY2": "value2",
	}

	env := NewFromMap(testMap)
	require.NotNil(t, env)
	require.IsType(t, &mapEnv{}, env)
}

func TestMapEnv_Get(t *testing.T) {
	testMap := map[string]string{
		"KEY1": "value1",
		"KEY2": "value2",
	}

	env := NewFromMap(testMap)

	// Test getting existing keys
	require.Equal(t, "value1", env.Get("KEY1"))
	require.Equal(t, "value2", env.Get("KEY2"))

	// Test getting non-existent key
	require.Equal(t, "", env.Get("NON_EXISTENT"))
}

func TestMapEnv_Env(t *testing.T) {
	t.Run("with values", func(t *testing.T) {
		testMap := map[string]string{
			"KEY1": "value1",
			"KEY2": "value2",
		}

		env := NewFromMap(testMap)
		envVars := env.Env()

		require.Len(t, envVars, 2)

		// Convert to map for easier testing (order is not guaranteed)
		envMap := make(map[string]string)
		for _, envVar := range envVars {
			parts := strings.SplitN(envVar, "=", 2)
			require.Len(t, parts, 2)
			envMap[parts[0]] = parts[1]
		}

		require.Equal(t, "value1", envMap["KEY1"])
		require.Equal(t, "value2", envMap["KEY2"])
	})

	t.Run("empty map", func(t *testing.T) {
		env := NewFromMap(map[string]string{})
		envVars := env.Env()
		require.NotNil(t, envVars)
		require.Len(t, envVars, 0)
	})

	t.Run("nil map", func(t *testing.T) {
		env := NewFromMap(nil)
		envVars := env.Env()
		require.NotNil(t, envVars)
		require.Len(t, envVars, 0)
	})
}

func TestMapEnv_GetEmptyValue(t *testing.T) {
	testMap := map[string]string{
		"EMPTY_KEY":  "",
		"NORMAL_KEY": "value",
	}

	env := NewFromMap(testMap)

	// Test that empty values are returned correctly
	require.Equal(t, "", env.Get("EMPTY_KEY"))
	require.Equal(t, "value", env.Get("NORMAL_KEY"))
}

func TestMapEnv_EnvFormat(t *testing.T) {
	testMap := map[string]string{
		"KEY_WITH_EQUALS": "value=with=equals",
		"KEY_WITH_SPACES": "value with spaces",
	}

	env := NewFromMap(testMap)
	envVars := env.Env()

	require.Len(t, envVars, 2)

	// Check that the format is correct even with special characters
	found := make(map[string]bool)
	for _, envVar := range envVars {
		if envVar == "KEY_WITH_EQUALS=value=with=equals" {
			found["equals"] = true
		}
		if envVar == "KEY_WITH_SPACES=value with spaces" {
			found["spaces"] = true
		}
	}

	require.True(t, found["equals"], "Should handle values with equals signs")
	require.True(t, found["spaces"], "Should handle values with spaces")
}

// TestOverlayEnv_ShadowsBaseKeys pins the core contract: an overlay key
// wins over the base for both Get and Env(), and a non-overlaid key
// passes through unchanged.
func TestOverlayEnv_ShadowsBaseKeys(t *testing.T) {
	base := NewFromMap(map[string]string{
		"FOO": "base-foo",
		"BAR": "base-bar",
	})
	o := NewOverlay(base, map[string]string{"FOO": "overlay-foo"})

	require.Equal(t, "overlay-foo", o.Get("FOO"), "overlay value must win")
	require.Equal(t, "base-bar", o.Get("BAR"), "non-overlaid key must pass through to base")
	require.Equal(t, "", o.Get("MISSING"))

	envList := o.Env()
	m := make(map[string]string)
	for _, kv := range envList {
		k, v, ok := strings.Cut(kv, "=")
		require.True(t, ok)
		m[k] = v
	}
	require.Equal(t, "overlay-foo", m["FOO"], "Env() must reflect the overlay value")
	require.Equal(t, "base-bar", m["BAR"])
	// Exactly one entry per key — no duplicate FOO from base+overlay.
	count := 0
	for _, kv := range envList {
		if strings.HasPrefix(kv, "FOO=") {
			count++
		}
	}
	require.Equal(t, 1, count, "Env() must not contain both the base and overlay value for a shadowed key")
}

// TestOverlayEnv_EmptyOverlayPassesThrough verifies a nil/empty overlay
// behaves as a pure pass-through to base, both for Get and Env().
func TestOverlayEnv_EmptyOverlayPassesThrough(t *testing.T) {
	base := NewFromMap(map[string]string{"FOO": "base-foo"})

	o := NewOverlay(base, nil)
	require.Equal(t, "base-foo", o.Get("FOO"))
	require.Equal(t, base.Env(), o.Env())
}

// TestOverlayEnv_NeverTouchesProcessEnviron is the P1.3 regression test:
// building and reading an overlay must never call os.Setenv/os.Unsetenv
// on the real process environment. It snapshots os.Environ() before and
// after exercising NewOverlay (Get and Env(), including a key that
// collides with a real process env var) and asserts the process
// environment is byte-for-byte unchanged.
func TestOverlayEnv_NeverTouchesProcessEnviron(t *testing.T) {
	// Pick a var name unlikely to collide with anything real, plus one
	// that likely already exists in most process environments (PATH) to
	// prove shadowing a real var doesn't leak into os.Environ() either.
	t.Setenv("CRUSH_ENV_OVERLAY_TEST_PROBE", "process-value")

	before := os.Environ()

	base := New() // osEnv — reads the real process environment.
	o := NewOverlay(base, map[string]string{
		"CRUSH_ENV_OVERLAY_TEST_PROBE": "overlay-value",
		"PATH":                         "/overlay/only/path",
	})

	require.Equal(t, "overlay-value", o.Get("CRUSH_ENV_OVERLAY_TEST_PROBE"))
	require.Equal(t, "/overlay/only/path", o.Get("PATH"))
	_ = o.Env()

	after := os.Environ()
	require.ElementsMatch(t, before, after, "NewOverlay/Get/Env must never mutate the real process environment")

	// Re-confirm via direct os.Getenv that the process-level value is
	// untouched (redundant with the ElementsMatch above, but pins the
	// specific historical failure mode: os.Setenv leaking a scoped
	// override into the process for every other goroutine/child process).
	require.Equal(t, "process-value", os.Getenv("CRUSH_ENV_OVERLAY_TEST_PROBE"))
}

// TestOverlayEnv_ConcurrentOverlaysDoNotInterfere fires many goroutines,
// each building its own overlay with a DIFFERENT value for the same key
// name, reading Get/Env repeatedly, and asserts each goroutine only ever
// observes its own overlay value — never another goroutine's. This is the
// P1.3 concurrency contract: unlike PushPopCrushEnv's process-global
// os.Setenv/os.Unsetenv, two concurrent overlays must not be able to
// observe or clobber each other, because there is no shared mutable state
// between them at all (each overlayEnv is an independent value closing
// over its own map).
func TestOverlayEnv_ConcurrentOverlaysDoNotInterfere(t *testing.T) {
	const goroutines = 20
	const iterations = 200

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("value-%d", i)
			o := NewOverlay(NewFromMap(map[string]string{"SHARED_KEY": "base-value"}), map[string]string{
				"SHARED_KEY": want,
			})
			for range iterations {
				require.Equal(t, want, o.Get("SHARED_KEY"))
				envList := o.Env()
				sharedCount := 0
				for _, kv := range envList {
					if !strings.HasPrefix(kv, "SHARED_KEY=") {
						continue
					}
					sharedCount++
					// Any other goroutine's value appearing here would be
					// a cross-goroutine leak — the only SHARED_KEY entry
					// must be exactly this goroutine's own value.
					require.Equal(t, "SHARED_KEY="+want, kv, "must never observe another goroutine's overlay value")
				}
				require.Equal(t, 1, sharedCount, "must see exactly one SHARED_KEY entry")
			}
		}(i)
	}
	wg.Wait()
}
