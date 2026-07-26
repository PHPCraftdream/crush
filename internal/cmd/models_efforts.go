// Fork patch: `crush models efforts [model]` — discoverability for reasoning
// effort. Before this command, the only place any of the following was
// documented was code comments in internal/agent/coordinator.go's
// getProviderOptions switch:
//
//   - There are two syntaxes for setting effort: the short codes (o47x,
//     h45l, ...) / long-form atom suffix (opus-high, glm5_2-max, ...) and the
//     raw "provider/model@effort" suffix.
//   - Effort-bearing letter short codes (o47x, h45l, ...) exist ONLY for the
//     local-cli/Claude atoms (see atomRegistry's EffortSource field in
//     models_atoms.go) — there is no "glm5_2xx". Z.AI atoms instead carry a
//     static ReasoningLevels array ({"off","high","max"}, see
//     zaiReasoningLevels in models_atoms.go) and accept the long-form
//     "<atom>-<level>" suffix, e.g. "glm5_2-max" (added alongside this
//     comment — previously any "-level" suffix on a Z.AI atom was rejected
//     outright).
//   - The raw "@effort" suffix (splitModelEffort in models_set.go) used to be
//     a blind string split with NO validation against the model's
//     ReasoningLevels anywhere. It is now validated (validateEffortForModel
//     in models_atoms.go) whenever the target (provider, model) resolves to
//     a known atom with a non-nil Levels() — a typo is rejected with an
//     error instead of silently yielding a wrong or ignored effort. Models
//     outside the atom registry still accept any string, unvalidated.
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
			"CROSS-CHECKED against Z.AI's own API reference (docs.z.ai/api-reference/",
			"llm/chat-completion): reasoning_effort is officially documented as",
			"\"Only supported by GLM-5.2\" among current models — every other GLM",
			"model (5.1, 5, 5-turbo, 4.7, 4.7-flash, 4.6) is documented as boolean",
			"thinking-toggle only, no graduated effort.",
			"",
			"The FORK's coordinator.go, however, currently still sends the same",
			"reasoning_effort wire value to every zai-routed model uniformly",
			"(collapsed to THREE actual wire states: off / high / max):",
			"  off                          -> thinking disabled",
			"  unset, low, medium, high     -> reasoning_effort: \"high\"",
			"  xhigh, max, ultracode        -> reasoning_effort: \"max\"",
			"low and medium are indistinguishable from high — setting them does",
			"nothing beyond what unset already gives you. Older GLM-4.x models",
			"presumably ignore the field harmlessly (undocumented as meaningful",
			"for them). This is a fork-level simplification, unchanged by this",
			"validation work — see the atom-level detail below for what's now",
			"actually validated per model.",
			"",
			"No letter short codes exist for Z.AI models (no `glm5_2xx`), but every",
			"Z.AI atom now declares a real, validated ReasoningLevels array — set",
			"with the long-form atom suffix (e.g. `glm5_2-max`) or the raw",
			"`zai/<model>@<level>` syntax (e.g. `zai/glm-5.2@max`); both are",
			"validated against that atom's specific list. glm5_2 validates against",
			"the documented 7-value reasoning_effort enum (none/minimal/low/medium/",
			"high/xhigh/max) plus \"off\" (a fork-level addition for fully disabling",
			"thinking, not part of Z.AI's reasoning_effort enum itself); every other",
			"Z.AI atom validates against boolean off/on only, matching Z.AI's",
			"documented per-model support matrix above.",
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
	b.WriteString("                    Long-form atom suffix also works for any atom with a\n")
	b.WriteString("                    known levels array, e.g. `crush models use glm5_2-max`.\n")
	b.WriteString("  2. Raw @effort    e.g. `crush models use zai/glm-5.2@max glm4_7`\n")
	b.WriteString("     For a model NOT in the atom registry, @effort is UNVALIDATED — it\n")
	b.WriteString("     is a blind string split (splitModelEffort), not checked against\n")
	b.WriteString("     the model's supported levels. A typo silently produces a wrong or\n")
	b.WriteString("     ignored effort instead of an error. For a KNOWN atom (e.g. any\n")
	b.WriteString("     glm5_2/opus/... atom), both syntaxes above ARE now validated\n")
	b.WriteString("     against that atom's real levels array and reject an unsupported\n")
	b.WriteString("     or typo'd level with a clear error.\n\n")

	b.WriteString("ASYMMETRY: effort-bearing LETTER short codes (o47x, h45l, ...) exist\n")
	b.WriteString("ONLY for the local-cli/Claude atoms (opus, opus46, opus47, opus48,\n")
	b.WriteString("sonnet, haiku, fable). Every other provider has no letter short code for effort\n")
	b.WriteString(" — e.g. there is no `glm5_2xx`. Z.AI models instead declare a\n")
	b.WriteString("real ReasoningLevels array and accept the long-form atom suffix\n")
	b.WriteString("(`glm5_2-max`) or the raw `provider/model@effort` syntax above, both\n")
	b.WriteString("validated. DeepSeek, io.net, Alibaba Singapore, and hyper models still\n")
	b.WriteString("have no atom-level validation and can only have their effort set with\n")
	b.WriteString("the unvalidated raw `provider/model@effort` syntax.\n\n")

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
		// coordinator.go switch this whole doc restates). Only GLM-5.2 has
		// real graduated reasoning_effort support per Z.AI's own API docs;
		// render straight from the resolved atom's real array instead of a
		// hardcoded copy. For a raw, non-atom zai/<model> the registry
		// doesn't know, fall back to the graduated list as the most
		// generically useful default — it's a documentation aid at that
		// point, not a validation source (validateEffortForModel only
		// validates known atoms).
		levels := zaiReasoningLevels
		isGraduated := true
		if a, ok := atomRegistry[target.AtomKey]; ok && a.ReasoningLevels != nil {
			levels = a.ReasoningLevels
			isGraduated = target.AtomKey == "glm5_2"
		}
		if isGraduated {
			b.WriteString("  GLM-5.2 is the ONLY Z.AI model with real graduated reasoning_effort\n")
			b.WriteString("  support (per Z.AI's own API docs). Default when thinking is enabled\n")
			b.WriteString("  but no effort is given is \"max\" (Z.AI's native default) — note the\n")
			b.WriteString("  fork's own coordinator.go instead defaults an unset effort to its\n")
			b.WriteString("  \"high\" wire value, a deliberate fork-level choice, not a Z.AI default.\n")
			tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
			for _, l := range levels {
				fmt.Fprintf(tw, "  %s\t crush models use %s/%s@%s <small>\n", l, target.Provider, target.Model, l)
			}
			tw.Flush()
		} else {
			b.WriteString("  This Z.AI model has NO documented graduated reasoning_effort support —\n")
			b.WriteString("  only GLM-5.2 does. It only exposes the boolean thinking toggle:\n")
			tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "  off\t crush models use %s/%s@off <small>\t(thinking disabled)\n", target.Provider, target.Model)
			fmt.Fprintf(tw, "  on\t crush models use %s/%s@on <small>\t(thinking enabled)\n", target.Provider, target.Model)
			tw.Flush()
			b.WriteString("  (coordinator.go currently still forwards a reasoning_effort value to\n")
			b.WriteString("  this model too — undocumented by Z.AI as meaningful here, presumably\n")
			b.WriteString("  harmless; that runtime behavior is unchanged by this validation.)\n")
		}
		if target.AtomKey != "" {
			fmt.Fprintf(&b, "  (These %d levels are validated — see `crush models use %s-<level>` above,\n", len(levels), target.AtomKey)
			b.WriteString("  or the raw @effort form below, which is validated against this same list.)\n")
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
