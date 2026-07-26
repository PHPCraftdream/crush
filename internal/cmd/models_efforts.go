// Fork patch: `crush models efforts [model]` — discoverability for reasoning
// effort.
//
//   - Two syntaxes set effort: short codes / long-form atom suffix
//     (opus-high, glm5_2-max, ...) and raw "provider/model@effort".
//   - Effort-bearing letter short codes (o47x, h45l, ...) exist ONLY for
//     local-cli/Claude atoms. Z.AI atoms carry a static ReasoningLevels
//     array instead (see models_atoms.go) and accept the long-form
//     "<atom>-<level>" suffix, e.g. "glm5_2-max".
//   - The raw "@effort" suffix is validated (validateEffortForModel in
//     models_atoms.go) against a known atom's Levels(); a typo is rejected.
//     Models outside the atom registry accept any string, unvalidated.
package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

// providerEffortDoc describes one provider (or provider-family)'s effort
// semantics in prose, for `crush models efforts` output.
//
// SYNC WARNING: this is a human-readable restatement of the effort-mapping
// logic in internal/agent/coordinator.go's getProviderOptions (the
// `case openaicompat.Name, hyper.Name:` switch on providerCfg.ID, plus the
// anthropic/bedrock and openai/azure branches). It is NOT derived from that
// switch — Go has no reflection-friendly way to turn "which case of a
// string switch fired" into documentation text. If you change the mapping
// in coordinator.go, you MUST update the matching entry here, or this
// command will lie to users. coordinator.go has a matching "SYNC WARNING"
// comment pointing back at this file.
type providerEffortDoc struct {
	// Key matches catwalk.InferenceProvider values (zai, deepseek, ionet,
	// alibaba-singapore, hyper) or "anthropic-cli" for the local-cli/Claude
	// family, which isn't a catwalk inference provider at all.
	Key   string
	Title string
	Body  []string
}

var providerEffortDocs = []providerEffortDoc{
	{
		Key:   "anthropic-cli",
		Title: "Claude models (local-cli provider: opus, sonnet, haiku, fable atoms)",
		Body: []string{
			"Effort levels are whatever the local `claude` CLI advertises via",
			"`claude --help` (cached per process; falls back to low/medium/high/xhigh/max",
			"if detection fails). ReasoningEffort is forwarded as-is as the CLI's own",
			"`--effort <level>` flag — the CLI binary validates it, not Crush.",
			"These are the ONLY atoms with effort-bearing short codes (o47x, h45l, sl, ...).",
		},
	},
	{
		Key:   string(catwalk.InferenceProviderZAI),
		Title: "Z.AI (all GLM models, e.g. glm5_2, glm5_1, glm4_7)",
		Body: []string{
			"Every Z.AI model sends the same wire values, collapsed to three",
			"real states:",
			"  off                          -> thinking disabled",
			"  unset, low, medium, high     -> reasoning_effort: \"high\"",
			"  xhigh, max, ultracode        -> reasoning_effort: \"max\"",
			"Older GLM-4.x models ignore reasoning_effort harmlessly.",
			"",
			"GLM-5.2 (glm5_2) is the only Z.AI atom with 3 real states",
			"(off/high/max — one more than the rest). Every other Z.AI atom",
			"(glm5_1, glm5, glm5_turbo, glm4_7, glm4_7_flash, glm4_6, glm4_6v)",
			"has 2 (off/on, a boolean thinking toggle). No letter short codes",
			"exist for Z.AI (no `glm5_2xx`) — set with the long-form atom suffix",
			"(`glm5_2-max`) or raw `zai/<model>@<level>` (`zai/glm-5.2@max`);",
			"both are validated against the atom's list.",
		},
	},
	{
		Key:   string(catwalk.InferenceProviderDeepSeek),
		Title: "DeepSeek",
		Body: []string{
			"Unlike Z.AI, an UNSET effort means thinking is OFF here (Z.AI defaults",
			"unset to \"high\"). Thinking turns on when Think is enabled or any",
			"ReasoningEffort is set. Once on, levels collapse the same way as Z.AI:",
			"  low, medium, high (default) -> \"high\"",
			"  xhigh, max, ultracode       -> \"max\"",
			"No short codes exist — use `deepseek/<model>@<level>`.",
		},
	},
	{
		Key:   string(catwalk.InferenceProviderIoNet),
		Title: "io.net",
		Body: []string{
			"No effort levels — only the model config's boolean Think field:",
			"  Think=true  -> reasoning.effort = \"medium\"",
			"  Think=false -> reasoning.effort = \"none\"",
			"The ReasoningEffort string (low/high/xhigh/...) is not read at all.",
			"No CLI flag sets Think today; edit the model's \"think\" field directly",
			"in crush.json.",
		},
	},
	{
		Key:   string(catwalk.InferenceProviderAlibabaSingapore),
		Title: "Alibaba Singapore",
		Body: []string{
			"Routes through one of two branches depending on the model's",
			"provider type:",
			"  anthropic/bedrock-routed: extra_body.reasoning_effort is sent",
			"    when ReasoningEffort is a member of the model's own",
			"    ReasoningLevels; otherwise extra_body.thinking mirrors Think.",
			"  openai-compat/hyper-routed: boolean only — extra_body.enable_thinking",
			"    mirrors Think; ReasoningEffort is not read at all.",
		},
	},
	{
		Key:   "hyper",
		Title: "hyper",
		Body: []string{
			"The Think boolean is passed straight through as `thinking` with no",
			"effort-level mapping at all.",
		},
	},
	{
		Key:   "openai-generic",
		Title: "OpenAI / Azure / OpenRouter / Vercel / generic openai-compat",
		Body: []string{
			"ReasoningEffort is forwarded as `reasoning_effort` (or the",
			"provider-specific equivalent field) only when it is a member of the",
			"model's own ReasoningLevels list (from provider/catwalk model data);",
			"otherwise it is silently dropped. Unlike Z.AI/DeepSeek there is no",
			"level collapsing here — whatever level the model advertises is sent",
			"verbatim.",
		},
	},
}

var modelsEffortsCmd = &cobra.Command{
	Use:   "efforts [model]",
	Short: "Explain reasoning-effort levels and how to set them, per provider or per model",
	Long: `What a reasoning-effort level actually does is provider-specific and
not visible from ` + "`crush models list`" + `.

Two syntaxes set effort:
  1. Short codes, e.g. ` + "`o47x`" + `, ` + "`h45l`" + `, ` + "`sh`" + ` (local-cli/Claude atoms only) or
     the long-form atom suffix, e.g. ` + "`glm5_2-max`" + `.
  2. Raw ` + "`provider/model@effort`" + `, e.g. ` + "`zai/glm-5.2@max`" + `. Validated when the
     target is a known atom; otherwise a blind, unvalidated string split.

Run with no argument for per-provider semantics. Run with a model or atom
argument (` + "`glm5_2`" + `, ` + "`zai/glm-5.2`" + `, ` + "`fl`" + `) for that model's exact levels and
the command to set each one.`,
	Args: cobra.MaximumNArgs(1),
	Example: `
# Per-provider semantics, syntaxes, and the Claude-only short-code asymmetry.
crush models efforts

# What does glm5_2 (Z.AI) support, and how do I set it?
crush models efforts glm5_2

# Same, addressed as raw provider/model.
crush models efforts zai/glm-5.2

# A Claude atom via its short-code base.
crush models efforts fl
crush models efforts fable
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			fmt.Print(renderEffortsOverview())
			return nil
		}
		out, err := renderEffortsForModel(args[0])
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	},
}

func effortDocByKey(key string) *providerEffortDoc {
	for i := range providerEffortDocs {
		if providerEffortDocs[i].Key == key {
			return &providerEffortDocs[i]
		}
	}
	return nil
}

func renderProviderDoc(b *strings.Builder, d providerEffortDoc) {
	b.WriteString("  " + d.Title + ":\n")
	for _, line := range d.Body {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("\n")
}

// renderEffortsOverview is the no-argument form: syntax, the Claude-only
// short-code asymmetry, and every provider's collapsing behavior.
func renderEffortsOverview() string {
	var b strings.Builder
	b.WriteString("REASONING EFFORT — how it's set and what it does\n\n")

	b.WriteString("SYNTAX:\n")
	b.WriteString("  1. Short codes    e.g. `crush models use o47x h45l` — local-cli/Claude only.\n")
	b.WriteString("                    Long-form atom suffix works for any atom with a known\n")
	b.WriteString("                    levels array, e.g. `crush models use glm5_2-max`.\n")
	b.WriteString("  2. Raw @effort    e.g. `crush models use zai/glm-5.2@max glm4_7`\n")
	b.WriteString("     Validated against the atom's real levels when the target is a\n")
	b.WriteString("     known atom (rejects a typo like `@hihg`); UNVALIDATED (blind string\n")
	b.WriteString("     split) for any model outside the atom registry.\n\n")

	b.WriteString("ASYMMETRY: LETTER short codes (o47x, h45l, ...) exist ONLY for the\n")
	b.WriteString("local-cli/Claude atoms (opus, opus46, opus47, opus48, sonnet, haiku,\n")
	b.WriteString("fable) — there is no `glm5_2xx`. Every other provider has no letter\n")
	b.WriteString("short code for effort; Z.AI atoms use the validated long-form atom\n")
	b.WriteString("suffix instead (`glm5_2-max`). DeepSeek, io.net, Alibaba Singapore,\n")
	b.WriteString("and hyper have no atom-level validation at all — only the unvalidated\n")
	b.WriteString("raw `provider/model@effort` syntax works for them.\n\n")

	b.WriteString("PER-PROVIDER SEMANTICS (what a level actually does):\n\n")
	for _, d := range providerEffortDocs {
		renderProviderDoc(&b, d)
	}

	b.WriteString("Run `crush models efforts <model>` (atom, short code, or provider/model)\n")
	b.WriteString("for that model's exact supported levels and command syntax.\n")
	return b.String()
}

// resolvedEffortTarget captures what we found for a `models efforts <arg>`
// lookup, independent of whether the arg was an atom key, a short code, or
// raw provider/model.
type resolvedEffortTarget struct {
	AtomKey     string // "" if not an atom
	Provider    string
	Model       string
	DisplayName string
}

// resolveEffortTarget accepts an atom key (glm5_2, fable, opus47), a
// short-code base without the effort suffix is NOT accepted here (short
// codes always include a level, e.g. "fl" not "f") — but a full short code
// like "fl" or "o47x" IS accepted and resolved to its underlying atom, or a
// raw "provider/model" string.
func resolveEffortTarget(arg string) (resolvedEffortTarget, bool) {
	// 1. Direct atom key.
	if a, ok := atomRegistry[arg]; ok {
		return resolvedEffortTarget{AtomKey: arg, Provider: a.Provider, Model: a.Model, DisplayName: a.DisplayName}, true
	}

	// 2. Full short code (e.g. "fl", "o47x", "h45l") — resolve to its atom.
	if sm, ok := parseShortCode(arg); ok {
		key := lookupAtomForModel(config.SelectedModel{Provider: sm.Provider, Model: sm.Model})
		display := sm.Model
		if key != "" {
			display = atomRegistry[key].DisplayName
		}
		return resolvedEffortTarget{AtomKey: key, Provider: sm.Provider, Model: sm.Model, DisplayName: display}, true
	}

	// 3. Raw "provider/model" (with optional @effort, ignored for lookup).
	if strings.Contains(arg, "/") {
		modelPart, _ := splitModelEffort(arg)
		idx := strings.Index(modelPart, "/")
		provider, model := modelPart[:idx], modelPart[idx+1:]
		if key := lookupAtomForModel(config.SelectedModel{Provider: provider, Model: model}); key != "" {
			return resolvedEffortTarget{AtomKey: key, Provider: provider, Model: model, DisplayName: atomRegistry[key].DisplayName}, true
		}
		return resolvedEffortTarget{Provider: provider, Model: model, DisplayName: model}, true
	}

	return resolvedEffortTarget{}, false
}

// providerDocKeyFor maps a resolved target's provider id to the
// providerEffortDocs key that documents it.
func providerDocKeyFor(provider string) string {
	if provider == "local-cli" {
		return "anthropic-cli"
	}
	switch provider {
	case string(catwalk.InferenceProviderZAI),
		string(catwalk.InferenceProviderDeepSeek),
		string(catwalk.InferenceProviderIoNet),
		string(catwalk.InferenceProviderAlibabaSingapore),
		"hyper":
		return provider
	default:
		return "openai-generic"
	}
}

func renderEffortsForModel(arg string) (string, error) {
	target, ok := resolveEffortTarget(arg)
	if !ok {
		return "", fmt.Errorf("%q is not a recognized atom, short code, or provider/model — see `crush models list`", arg)
	}

	var b strings.Builder
	label := target.DisplayName
	if target.AtomKey != "" {
		label = fmt.Sprintf("%s (atom: %s)", target.DisplayName, target.AtomKey)
	}
	fmt.Fprintf(&b, "%s — %s/%s\n\n", label, target.Provider, target.Model)

	docKey := providerDocKeyFor(target.Provider)
	if d := effortDocByKey(docKey); d != nil {
		b.WriteString("PROVIDER SEMANTICS:\n")
		renderProviderDoc(&b, *d)
	}

	b.WriteString("HOW TO SET EACH LEVEL FOR THIS MODEL:\n\n")

	if target.Provider == "local-cli" {
		a, hasAtom := atomRegistry[target.AtomKey]
		if hasAtom && a.EffortSource != nil {
			levels := a.EffortSource.Levels()
			tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
			for _, l := range levels {
				fmt.Fprintf(tw, "  %s-%s\t or  crush models use %s-%s <small>\n", target.AtomKey, l, target.AtomKey, l)
			}
			tw.Flush()
			b.WriteString("\n  (Levels detected from `claude --help`; falls back to a fixed\n")
			b.WriteString("  low/medium/high/xhigh/max list if the CLI can't be reached.)\n")
		} else {
			b.WriteString("  This local-cli model was not found in the atom registry with an\n")
			b.WriteString("  effort source; use `crush models use local-cli/" + target.Model + "@<level> <small>`.\n")
		}
		return b.String(), nil
	}

	// Non-Claude: raw @effort syntax is the only option. List candidate
	// levels from provider docs where we know them; otherwise show the
	// generic form only.
	fmt.Fprintf(&b, "  crush models use %s/%s@<level> <small>\n\n", target.Provider, target.Model)

	// Z.AI atoms additionally now support the long-form "<atom>-<level>"
	// suffix (validated against ReasoningLevels), same mechanism Claude
	// atoms already use for their EffortSource-detected levels.
	if a, ok := atomRegistry[target.AtomKey]; ok && a.ReasoningLevels != nil && providerDocKeyFor(target.Provider) == string(catwalk.InferenceProviderZAI) {
		fmt.Fprintf(&b, "  Or the validated long-form atom suffix: crush models use %s-<level> <small>\n\n", target.AtomKey)
	}

	switch providerDocKeyFor(target.Provider) {
	case string(catwalk.InferenceProviderZAI):
		// SYNC WARNING: which levels apply to which Z.AI model restates
		// zaiReasoningLevels / zaiBooleanThinkingLevels in models_atoms.go
		// (themselves paired, via their own SYNC WARNING, with the
		// coordinator.go switch this whole doc restates). Render straight
		// from the resolved atom's real array instead of a hardcoded copy.
		// For a raw, non-atom zai/<model> the registry doesn't know, fall
		// back to zaiReasoningLevels (glm5_2's 3-state array) as the most
		// generically useful default — a documentation aid, not a
		// validation source (validateEffortForModel only validates known
		// atoms).
		levels := zaiReasoningLevels
		if a, ok := atomRegistry[target.AtomKey]; ok && a.ReasoningLevels != nil {
			levels = a.ReasoningLevels
		}
		if target.AtomKey == "glm5_2" {
			b.WriteString("  GLM-5.2 is the only Z.AI atom with 3 real states (the rest have 2):\n")
		} else {
			b.WriteString("  This Z.AI model only exposes the boolean thinking toggle:\n")
		}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, l := range levels {
			fmt.Fprintf(tw, "  %s\t crush models use %s/%s@%s <small>\n", l, target.Provider, target.Model, l)
		}
		tw.Flush()
		if target.AtomKey != "" {
			fmt.Fprintf(&b, "  (Validated — see `crush models use %s-<level>` above, or the raw\n", target.AtomKey)
			b.WriteString("  @effort form, validated against this same list.)\n")
		}
	case string(catwalk.InferenceProviderDeepSeek):
		b.WriteString("  Meaningful levels for this provider (others collapse into these):\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  (unset)\t crush models use %s/%s <small>\t(thinking OFF — different from Z.AI)\n", target.Provider, target.Model)
		fmt.Fprintf(tw, "  high\t crush models use %s/%s@high <small>\t(also: low, medium)\n", target.Provider, target.Model)
		fmt.Fprintf(tw, "  max\t crush models use %s/%s@max <small>\t(also: xhigh, ultracode)\n", target.Provider, target.Model)
		tw.Flush()
	case string(catwalk.InferenceProviderIoNet):
		b.WriteString("  This provider ignores @effort entirely — it only reads the Think\n")
		b.WriteString("  boolean (medium if on, none if off). No @level syntax applies.\n")
	case string(catwalk.InferenceProviderAlibabaSingapore):
		b.WriteString("  This provider mostly ignores @effort in favor of the Think boolean\n")
		b.WriteString("  (enable_thinking). See provider semantics above.\n")
	case "hyper":
		b.WriteString("  This provider ignores @effort entirely — only the Think boolean is\n")
		b.WriteString("  forwarded. No @level syntax applies.\n")
	default:
		b.WriteString("  Valid levels are whatever this model's ReasoningLevels advertises;\n")
		b.WriteString("  see `crush models list` (\"reason:\" column) for this specific model.\n")
	}
	if a, ok := atomRegistry[target.AtomKey]; ok && a.Levels() != nil {
		b.WriteString("\n  This model is a known atom, so @effort (and the atom-suffix form\n")
		b.WriteString("  above, if shown) IS validated against the levels listed above —\n")
		b.WriteString("  an unsupported level is now rejected with an error, not silently\n")
		b.WriteString("  accepted.\n")
	} else {
		b.WriteString("\n  Remember: this model isn't in the atom registry, so @effort is\n")
		b.WriteString("  unvalidated here — an unsupported level is accepted syntactically\n")
		b.WriteString("  and either ignored or silently mismapped by the provider.\n")
	}

	return b.String(), nil
}

func init() {
	modelsCmd.AddCommand(modelsEffortsCmd)
}
