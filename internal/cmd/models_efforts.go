// Fork patch: `crush models efforts [model]` — discoverability for reasoning
// effort. Before this command, the only place any of the following was
// documented was code comments in internal/agent/coordinator.go's
// getProviderOptions switch:
//
//   - There are two syntaxes for setting effort: the short codes (o47x,
//     h45l, ...) and the raw "provider/model@effort" suffix.
//   - Effort-bearing short codes exist ONLY for the local-cli/Claude atoms
//     (see atomRegistry's EffortSource field in models_atoms.go). A Z.AI
//     model's effort can only be set with the raw "@effort" form — there is
//     no "glm5_2xx".
//   - The raw "@effort" suffix (splitModelEffort in models_set.go) is a
//     blind string split with no validation against the model's
//     ReasoningLevels. A typo silently yields a wrong or ignored effort.
//   - What an effort actually DOES is provider-specific and sometimes
//     collapses distinct levels together (e.g. Z.AI: low/medium/high/unset
//     all map to the same "high" wire value; only xhigh/max/ultracode reach
//     "max"). This is the single most surprising fact for users and the one
//     most likely to waste their time if undiscovered.
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
			"Z.AI exposes only THREE actual wire states: off / high / max.",
			"  off                          -> thinking disabled",
			"  unset, low, medium, high     -> reasoning_effort: \"high\"",
			"  xhigh, max, ultracode        -> reasoning_effort: \"max\"",
			"low and medium are indistinguishable from high — setting them does",
			"nothing beyond what unset already gives you. Older GLM-4.x models",
			"ignore the effort field entirely (harmlessly).",
			"No short codes exist for Z.AI models — effort can only be set with",
			"the raw `zai/<model>@<level>` syntax, e.g. `zai/glm-5.2@max`.",
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
			"No effort levels — only a boolean Think flag, mapped to:",
			"  Think=true  -> reasoning.effort = \"medium\"",
			"  Think=false -> reasoning.effort = \"none\"",
			"The ReasoningEffort string (low/high/xhigh/...) is not read at all.",
			"Set with `--think` on the model, not a `@level` suffix.",
		},
	},
	{
		Key:   string(catwalk.InferenceProviderAlibabaSingapore),
		Title: "Alibaba Singapore",
		Body: []string{
			"Boolean only: `enable_thinking` mirrors the Think flag. No effort",
			"level is honored here (a ReasoningEffort value present in the model's",
			"ReasoningLevels list is instead sent via extra_body.reasoning_effort,",
			"a separate branch from the effort-collapsing providers above).",
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
	Long: `Reasoning effort ("how hard should the model think") is configured two
different ways in Crush, and what a given level actually DOES is
provider-specific — none of this is visible from ` + "`crush models list`" + ` alone.

Two syntaxes set effort:
  1. Short codes, e.g. ` + "`o47x`" + `, ` + "`h45l`" + `, ` + "`sh`" + ` — an atom+level baked into one
     token. See ` + "`crush models list`" + ` for the short-code table.
  2. Raw ` + "`provider/model@effort`" + `, e.g. ` + "`zai/glm-5.2@max`" + `. The ` + "`@effort`" + `
     suffix is a blind string split — NOT validated against the model's
     supported levels. A typo (` + "`@hihg`" + `) silently produces a wrong or
     ignored effort instead of an error.

Short codes exist ONLY for the local-cli/Claude atoms (opus, sonnet, haiku,
fable). Every other provider — Z.AI, DeepSeek, io.net, Alibaba Singapore,
hyper — has no short code for effort at all; the raw @effort syntax is the
only way to set it, and it is unvalidated as described above.

Run with no argument to print per-provider semantics (what levels exist,
what unset resolves to, which levels collapse into which — Z.AI's
low/medium/high all collapsing to the same wire value is the single most
surprising one). Run with a model or atom argument (` + "`glm5_2`" + `,
` + "`zai/glm-5.2`" + `, ` + "`fl`" + `) to see that model's levels and the exact command to
set each one.`,
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

	b.WriteString("SYNTAX (two ways to set effort):\n")
	b.WriteString("  1. Short codes    e.g. `crush models use o47x h45l`  (atom+level, one token)\n")
	b.WriteString("  2. Raw @effort    e.g. `crush models use zai/glm-5.2@max glm4_7`\n")
	b.WriteString("     The @effort suffix is UNVALIDATED — it is a blind string split\n")
	b.WriteString("     (splitModelEffort), not checked against the model's supported\n")
	b.WriteString("     levels. A typo silently produces a wrong or ignored effort\n")
	b.WriteString("     instead of an error.\n\n")

	b.WriteString("ASYMMETRY: effort-bearing short codes exist ONLY for the local-cli/\n")
	b.WriteString("Claude atoms (opus, opus46, opus47, opus48, sonnet, haiku, fable).\n")
	b.WriteString("Every other provider has no short code for effort — e.g. there is no\n")
	b.WriteString("`glm5_2xx`. Z.AI, DeepSeek, io.net, Alibaba Singapore, and hyper models\n")
	b.WriteString("can only have their effort set with the raw `provider/model@effort`\n")
	b.WriteString("syntax above.\n\n")

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
	switch providerDocKeyFor(target.Provider) {
	case string(catwalk.InferenceProviderZAI):
		b.WriteString("  Meaningful levels for this provider (others collapse into these):\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  off\t crush models use %s/%s@off <small>\t(thinking disabled)\n", target.Provider, target.Model)
		fmt.Fprintf(tw, "  high\t crush models use %s/%s@high <small>\t(also: unset, low, medium)\n", target.Provider, target.Model)
		fmt.Fprintf(tw, "  max\t crush models use %s/%s@max <small>\t(also: xhigh, ultracode)\n", target.Provider, target.Model)
		tw.Flush()
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
	b.WriteString("\n  Remember: @effort is unvalidated — an unsupported level is accepted\n")
	b.WriteString("  syntactically and either ignored or silently mismapped.\n")

	return b.String(), nil
}

func init() {
	modelsCmd.AddCommand(modelsEffortsCmd)
}
