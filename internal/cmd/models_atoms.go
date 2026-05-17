package cmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/config"
)

type atom struct {
	Provider    string
	Model       string
	DisplayName string
	CtxLabel    string
	Group       string
	GroupNote   string
	Vision      bool
	EffortSource *cliEffortSource
}

var atomRegistry = map[string]atom{
	"opus":         {Provider: "local-cli", Model: "cli-claude-opus", DisplayName: "Claude Opus", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"sonnet":       {Provider: "local-cli", Model: "cli-claude-sonnet", DisplayName: "Claude Sonnet", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"haiku":        {Provider: "local-cli", Model: "cli-claude-haiku", DisplayName: "Claude Haiku", CtxLabel: "200k", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"glm5_1":       {Provider: "zai", Model: "glm-5.1", DisplayName: "GLM 5.1", CtxLabel: "204.8k", Group: "zai", GroupNote: "openai-compat, no effort"},
	"glm5":         {Provider: "zai", Model: "glm-5", DisplayName: "GLM 5", CtxLabel: "204.8k", Group: "zai"},
	"glm5_turbo":   {Provider: "zai", Model: "glm-5-turbo", DisplayName: "GLM 5 turbo", CtxLabel: "200k", Group: "zai"},
	"glm4_7":       {Provider: "zai", Model: "glm-4.7", DisplayName: "GLM 4.7", CtxLabel: "204.8k", Group: "zai"},
	"glm4_7_flash": {Provider: "zai", Model: "glm-4.7-flash", DisplayName: "GLM 4.7 flash", CtxLabel: "204.8k", Group: "zai"},
	"glm4_6":       {Provider: "zai", Model: "glm-4.6", DisplayName: "GLM 4.6", CtxLabel: "204.8k", Group: "zai"},
	"glm4_6v":      {Provider: "zai", Model: "glm-4.6v", DisplayName: "GLM 4.6v", CtxLabel: "204.8k", Group: "zai", Vision: true},
	"glm4_5":       {Provider: "zai", Model: "glm-4.5", DisplayName: "GLM 4.5", CtxLabel: "131.1k", Group: "zai"},
	"glm4_5_air":   {Provider: "zai", Model: "glm-4.5-air", DisplayName: "GLM 4.5 air", CtxLabel: "204.8k", Group: "zai"},
	"glm4_5v":      {Provider: "zai", Model: "glm-4.5v", DisplayName: "GLM 4.5v", CtxLabel: "?", Group: "zai", Vision: true},
}

var atomGroupOrder = []string{"anthropic", "zai"}

// atomDisplayOrder pins the row order inside each group's output. Longest-first
// is still required for the parser (sortedAtomKeysForParse), but a fixed
// display order keeps the human-readable list predictable (Opus first, then
// Sonnet, then Haiku; newest GLM first, then descending versions).
var atomDisplayOrder = map[string][]string{
	"anthropic": {"opus", "sonnet", "haiku"},
	"zai": {
		"glm5_1", "glm5", "glm5_turbo",
		"glm4_7", "glm4_7_flash",
		"glm4_6", "glm4_6v",
		"glm4_5", "glm4_5_air", "glm4_5v",
	},
}

// sortedAtomKeys returns atom keys sorted longest-first for use by the parser
// (so "glm5_turbo" wins over "glm5" as a prefix). NOT used for display order.
func sortedAtomKeys() []string {
	keys := make([]string, 0, len(atomRegistry))
	for k := range atomRegistry {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// atomsByGroup returns the display order for a group (atomDisplayOrder),
// falling back to longest-first sort for any keys missing from the explicit
// list (defensive — keeps newly-added atoms visible without crash).
func atomsByGroup(group string) []string {
	want := atomDisplayOrder[group]
	seen := map[string]bool{}
	var out []string
	for _, k := range want {
		if _, ok := atomRegistry[k]; ok && atomRegistry[k].Group == group {
			out = append(out, k)
			seen[k] = true
		}
	}
	// Catch-all for any atoms not yet listed in atomDisplayOrder.
	for _, k := range sortedAtomKeys() {
		if atomRegistry[k].Group == group && !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func enabledAtomKeys(cfg *config.Config) []string {
	enabled := map[string]bool{}
	for _, p := range cfg.EnabledProviders() {
		enabled[p.ID] = true
	}
	var keys []string
	for _, k := range sortedAtomKeys() {
		a := atomRegistry[k]
		if enabled[a.Provider] {
			keys = append(keys, k)
		}
	}
	return keys
}

func enabledGroupAtomKeys(cfg *config.Config, group string) []string {
	enabled := map[string]bool{}
	for _, p := range cfg.EnabledProviders() {
		enabled[p.ID] = true
	}
	var keys []string
	for _, k := range atomsByGroup(group) {
		a := atomRegistry[k]
		if enabled[a.Provider] {
			keys = append(keys, k)
		}
	}
	return keys
}

func formatAtomLine(w io.Writer, key string, a atom) {
	if a.EffortSource != nil {
		levels := a.EffortSource.Levels()
		names := make([]string, len(levels))
		for i, l := range levels {
			names[i] = key + "-" + l
		}
		var suffix string
		if a.Vision {
			suffix = ", vision"
		}
		fmt.Fprintf(w, "    %s\t%s\t(%s ctx%s)\n", strings.Join(names, ", "), a.DisplayName, a.CtxLabel, suffix)
	} else {
		var suffix string
		if a.Vision {
			suffix = ", vision"
		}
		fmt.Fprintf(w, "    %s\t%s\t(%s ctx%s)\n", key, a.DisplayName, a.CtxLabel, suffix)
	}
}

func renderAtomsBlock(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("ATOMS (combine as `crush models use <large> <small>`):\n\n")
	for _, group := range atomGroupOrder {
		keys := enabledGroupAtomKeys(cfg, group)
		if len(keys) == 0 {
			continue
		}
		label := strings.Title(group)
		b.WriteString("  " + label + ":\n")
		note := ""
		for _, k := range keys {
			a := atomRegistry[k]
			if a.GroupNote != "" {
				note = a.GroupNote
				break
			}
		}
		if note != "" {
			b.WriteString("    " + note + "\n")
		}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, k := range keys {
			formatAtomLine(tw, k, atomRegistry[k])
		}
		tw.Flush()
		b.WriteString("\n")
	}
	b.WriteString("EXAMPLES:\n  crush models use opus-high sonnet-low\n  crush models use glm5_1 glm5_turbo\n  crush models use opus-max glm5_turbo\n")
	return b.String()
}

func renderAtomsBlockFallback() string {
	var b strings.Builder
	b.WriteString("ATOMS (combine as `crush models use <large> <small>`):\n\n")
	for _, group := range atomGroupOrder {
		keys := atomsByGroup(group)
		if len(keys) == 0 {
			continue
		}
		label := strings.Title(group)
		b.WriteString("  " + label + ":\n")
		note := ""
		for _, k := range keys {
			a := atomRegistry[k]
			if a.GroupNote != "" {
				note = a.GroupNote
				break
			}
		}
		if note != "" {
			b.WriteString("    " + note + "\n")
		}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, k := range keys {
			formatAtomLine(tw, k, atomRegistry[k])
		}
		tw.Flush()
		b.WriteString("\n")
	}
	b.WriteString("EXAMPLES:\n  crush models use opus-high sonnet-low\n  crush models use glm5_1 glm5_turbo\n  crush models use opus-max glm5_turbo\n")
	return b.String()
}

// parseAtom takes a string like "opus-high" or "glm5_turbo" or, as fallback,
// "openai/gpt-5@high" / "zai/glm-5.1". Returns a SelectedModel ready for
// UpdatePreferredModel.
func parseAtom(name string) (config.SelectedModel, error) {
	if strings.Contains(name, "/") {
		modelPart, effort := splitModelEffort(name)
		return config.SelectedModel{Provider: "", Model: modelPart, ReasoningEffort: effort}, fmt.Errorf("raw provider/model not yet resolved: %s", name)
	}

	var matchedKey string
	for _, k := range sortedAtomKeys() {
		if strings.HasPrefix(name, k) {
			matchedKey = k
			break
		}
	}
	if matchedKey == "" {
		return config.SelectedModel{}, fmt.Errorf("%q is not a recognized atom — see `crush models list`", name)
	}

	a := atomRegistry[matchedKey]
	rem := name[len(matchedKey):]

	if a.EffortSource != nil {
		if rem == "" {
			return config.SelectedModel{}, fmt.Errorf("%s requires explicit level (e.g. %s-low, %s-high) — see `crush models list`", matchedKey, matchedKey, matchedKey)
		}
		if !strings.HasPrefix(rem, "-") {
			return config.SelectedModel{}, fmt.Errorf("%q is not a recognized atom — see `crush models list`", name)
		}
		level := rem[1:]
		levels := a.EffortSource.Levels()
		valid := false
		for _, l := range levels {
			if l == level {
				valid = true
				break
			}
		}
		if !valid {
			return config.SelectedModel{}, fmt.Errorf("%q is not a valid level for %s (valid: %s)", level, matchedKey, strings.Join(levels, "|"))
		}
		return config.SelectedModel{
			Provider:        a.Provider,
			Model:           a.Model,
			ReasoningEffort: level,
		}, nil
	}

	if rem != "" {
		return config.SelectedModel{}, fmt.Errorf("%s does not support effort levels (provider %s) — unexpected suffix %q", matchedKey, a.Provider, rem)
	}

	return config.SelectedModel{
		Provider: a.Provider,
		Model:    a.Model,
	}, nil
}

// parseAtomOrRaw tries the atom registry first, then falls back to raw
// provider/model resolution via the app's ResolveModel.
func parseAtomOrRaw(name string, resolveFunc func(string) (string, string, error)) (config.SelectedModel, error) {
	if strings.Contains(name, "/") {
		modelPart, effort := splitModelEffort(name)
		provider, modelID, err := resolveFunc(modelPart)
		if err != nil {
			return config.SelectedModel{}, err
		}
		return config.SelectedModel{
			Provider:        provider,
			Model:           modelID,
			ReasoningEffort: effort,
		}, nil
	}

	sm, err := parseAtom(name)
	if err == nil {
		return sm, nil
	}

	if !strings.Contains(err.Error(), "not a recognized atom") {
		return config.SelectedModel{}, err
	}

	modelPart, effort := splitModelEffort(name)
	provider, modelID, rerr := resolveFunc(modelPart)
	if rerr != nil {
		return config.SelectedModel{}, fmt.Errorf("%q is not a known atom or provider/model — see `crush models list`", name)
	}
	return config.SelectedModel{
		Provider:        provider,
		Model:           modelID,
		ReasoningEffort: effort,
	}, nil
}

func renderAtomsBlockToStdout() {
	// Best-effort: try to load config for filtering, fall back to full list.
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprint(os.Stdout, renderAtomsBlockFallback())
		return
	}
	cfg, err := loadConfigForList(cwd)
	if err != nil || cfg == nil {
		fmt.Fprint(os.Stdout, renderAtomsBlockFallback())
		return
	}
	fmt.Fprint(os.Stdout, renderAtomsBlock(cfg))
}

func loadConfigForList(cwd string) (*config.Config, error) {
	store, err := config.Init(cwd, "", false)
	if err != nil {
		return nil, err
	}
	return store.Config(), nil
}

func lookupAtomForModel(sm config.SelectedModel) string {
	for k, a := range atomRegistry {
		if a.Provider == sm.Provider && a.Model == sm.Model {
			return k
		}
	}
	return ""
}
