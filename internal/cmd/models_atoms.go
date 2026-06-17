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
	Provider     string
	Model        string
	DisplayName  string
	CtxLabel     string
	Group        string
	GroupNote    string
	Vision       bool
	EffortSource *cliEffortSource
}

var atomRegistry = map[string]atom{
	"opus":         {Provider: "local-cli", Model: "cli-claude-opus-4-8", DisplayName: "Claude Opus 4.8", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"opus46":       {Provider: "local-cli", Model: "cli-claude-opus-4-6", DisplayName: "Claude Opus 4.6", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"opus47":       {Provider: "local-cli", Model: "cli-claude-opus-4-7", DisplayName: "Claude Opus 4.7", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"opus48":       {Provider: "local-cli", Model: "cli-claude-opus-4-8", DisplayName: "Claude Opus 4.8", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"sonnet":       {Provider: "local-cli", Model: "cli-claude-sonnet", DisplayName: "Claude Sonnet 4.6", CtxLabel: "1M", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"haiku":        {Provider: "local-cli", Model: "cli-claude-haiku", DisplayName: "Claude Haiku 4.5", CtxLabel: "200k", Group: "anthropic", GroupNote: "via local `claude` CLI", EffortSource: claudeEffortSource},
	"fable":        {Provider: "local-cli", Model: "cli-claude-fable", DisplayName: "Claude Fable 5", CtxLabel: "1M", Group: "anthropic", EffortSource: claudeEffortSource},
	"glm5_2":       {Provider: "zai", Model: "glm-5.2", DisplayName: "GLM 5.2", CtxLabel: "1M", Group: "zai", GroupNote: "openai-compat, no effort"},
	"glm5_1":       {Provider: "zai", Model: "glm-5.1", DisplayName: "GLM 5.1", CtxLabel: "204.8k", Group: "zai"},
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
	"anthropic": {"opus", "opus48", "opus47", "opus46", "fable", "sonnet", "haiku"},
	"zai": {
		"glm5_2", "glm5_1", "glm5", "glm5_turbo",
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
		label := titleCase(group)
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
	b.WriteString(renderShortCodesBlock())
	b.WriteString("EXAMPLES:\n")
	b.WriteString("  crush models use o47x h45l       # Opus 4.7 xhigh + Haiku 4.5 low\n")
	b.WriteString("  crush models use s46h h45l       # Sonnet 4.6 high + Haiku 4.5 low\n")
	b.WriteString("  crush models use opus-high sonnet-low\n")
	b.WriteString("  crush models use glm5_1 glm5_turbo\n")
	b.WriteString("  crush models use ox glm5_turbo    # mixed\n")
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
		label := titleCase(group)
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
	b.WriteString(renderShortCodesBlock())
	b.WriteString("EXAMPLES:\n")
	b.WriteString("  crush models use o47x h45l\n")
	b.WriteString("  crush models use s46h h45l\n")
	b.WriteString("  crush models use glm5_1 glm5_turbo\n")
	return b.String()
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// renderShortCodesBlock returns a formatted table of the short-code aliases.
func renderShortCodesBlock() string {
	var b strings.Builder
	b.WriteString("SHORT CODES (alias for atom+effort, e.g. `crush models use o47x h45l`):\n\n")
	b.WriteString("  Code     Model               CTX   Effort\n")
	b.WriteString("  -------  ------------------  ----  ------\n")
	rows := []struct{ code, model, ctx, effort string }{
		// Versioned
		{"o47l", "claude-opus-4-7", "1M", "low"},
		{"o47m", "claude-opus-4-7", "1M", "medium"},
		{"o47h", "claude-opus-4-7", "1M", "high"},
		{"o47x", "claude-opus-4-7", "1M", "xhigh"},
		{"o47xx", "claude-opus-4-7", "1M", "max"},
		{"o46l", "claude-opus-4-6", "1M", "low"},
		{"o46m", "claude-opus-4-6", "1M", "medium"},
		{"o46h", "claude-opus-4-6", "1M", "high"},
		{"o46xx", "claude-opus-4-6", "1M", "max"},
		{"s46l", "claude-sonnet-4-6", "200k", "low"},
		{"s46m", "claude-sonnet-4-6", "200k", "medium"},
		{"s46h", "claude-sonnet-4-6", "200k", "high"},
		{"s46xx", "claude-sonnet-4-6", "200k", "max"},
		{"s45l", "claude-sonnet-4-5", "200k", "low"},
		{"s45m", "claude-sonnet-4-5", "200k", "medium"},
		{"s45h", "claude-sonnet-4-5", "200k", "high"},
		{"h45l", "claude-haiku-4-5", "200k", "low"},
		{"h45m", "claude-haiku-4-5", "200k", "medium"},
		{"h45h", "claude-haiku-4-5", "200k", "high"},
		// Top-model shortcuts
		{"ol", "claude-opus-4-8", "1M", "low"},
		{"om", "claude-opus-4-8", "1M", "medium"},
		{"oh", "claude-opus-4-8", "1M", "high"},
		{"ox", "claude-opus-4-8", "1M", "xhigh"},
		{"oxx", "claude-opus-4-8", "1M", "max"},
		{"sl", "claude-sonnet-4-6", "200k", "low"},
		{"sm", "claude-sonnet-4-6", "200k", "medium"},
		{"sh", "claude-sonnet-4-6", "200k", "high"},
		{"sx", "claude-sonnet-4-6", "200k", "max"},
		{"hl", "claude-haiku-4-5", "200k", "low"},
		{"hm", "claude-haiku-4-5", "200k", "medium"},
		{"hh", "claude-haiku-4-5", "200k", "high"},
		{"fl", "claude-fable-5", "1M", "low"},
		{"fm", "claude-fable-5", "1M", "medium"},
		{"fh", "claude-fable-5", "1M", "high"},
		{"fx", "claude-fable-5", "1M", "xhigh"},
		{"fxx", "claude-fable-5", "1M", "max"},
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.code, r.model, r.ctx, r.effort)
	}
	tw.Flush()
	b.WriteString("\n")
	return b.String()
}

// shortCodeEffort maps the letter suffix to an effort level name.
var shortCodeEffort = map[string]string{
	"l":  "low",
	"m":  "medium",
	"h":  "high",
	"x":  "xhigh",
	"xx": "max",
}

// shortCodeBase maps the model prefix part of a short code to the
// corresponding atom key in atomRegistry. Includes both versioned
// prefixes (o47, s46, …) and top-model shortcuts (o, s, h).
var shortCodeBase = map[string]string{
	"o48": "opus48",
	"o47": "opus47",
	"o46": "opus46",
	"s46": "sonnet",
	"s45": "sonnet",
	"h45": "haiku",
	"o":   "opus",
	"s":   "sonnet",
	"h":   "haiku",
	"f":   "fable",
}

// shortCodeValidEfforts lists which effort suffixes each base accepts.
var shortCodeValidEfforts = map[string][]string{
	"o48": {"l", "m", "h", "x", "xx"},
	"o47": {"l", "m", "h", "x", "xx"},
	"o46": {"l", "m", "h", "x", "xx"},
	"s46": {"l", "m", "h", "xx"},
	"s45": {"l", "m", "h"},
	"h45": {"l", "m", "h"},
	"o":   {"l", "m", "h", "x", "xx"},
	"s":   {"l", "m", "h", "xx"},
	"h":   {"l", "m", "h"},
	"f":   {"l", "m", "h", "x", "xx"},
}

// parseShortCode tries to parse a short-code atom like "o47x" or "h45l".
// Returns ok=false if the input doesn't match the pattern.
func parseShortCode(name string) (config.SelectedModel, bool) {
	// Try to split into (base, suffix) by testing known bases longest-first.
	// "o47xx" → base="o47", suffix="xx"; "ol" → base="o", suffix="l".
	for _, base := range shortCodeBasesByLength {
		if !strings.HasPrefix(name, base) {
			continue
		}
		suffix := name[len(base):]
		if suffix == "" {
			continue
		}
		atomKey, ok := shortCodeBase[base]
		if !ok {
			continue
		}
		effort, ok := shortCodeEffort[suffix]
		if !ok {
			continue
		}
		// Validate effort is allowed for this base.
		if !isValidEffort(base, suffix) {
			return config.SelectedModel{}, false
		}
		a, ok := atomRegistry[atomKey]
		if !ok {
			return config.SelectedModel{}, false
		}
		return config.SelectedModel{
			Provider:        a.Provider,
			Model:           a.Model,
			ReasoningEffort: effort,
		}, true
	}
	return config.SelectedModel{}, false
}

// isValidEffort checks whether the effort suffix is valid for the given base.
func isValidEffort(base, suffix string) bool {
	valid, ok := shortCodeValidEfforts[base]
	if !ok {
		return false
	}
	for _, v := range valid {
		if v == suffix {
			return true
		}
	}
	return false
}

// shortCodeBasesByLength lists short-code bases sorted longest-first so
// that "o47" is tried before "o" during parsing.
var shortCodeBasesByLength = func() []string {
	keys := make([]string, 0, len(shortCodeBase))
	for k := range shortCodeBase {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}()

// parseAtom takes a string like "o47x", "opus-high", "glm5_turbo", or, as
// fallback, "openai/gpt-5@high" / "zai/glm-5.1". Returns a SelectedModel
// ready for UpdatePreferredModel.
func parseAtom(name string) (config.SelectedModel, error) {
	// Try short-code notation first (o47x, h45l, oh, sl, …).
	if sm, ok := parseShortCode(name); ok {
		return sm, nil
	}
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
