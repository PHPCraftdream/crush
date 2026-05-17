// Fork patch: batch 11 — `crush models list` prints the atom registry filtered
// by enabled providers + a section of raw provider/model IDs for everything
// else available right now. Replaces the implicit "discoverability via
// `crush models <fuzzy>` only" path.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
)

var modelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the atom registry plus all models from enabled providers",
	Long: `Print the atom registry — short, friendly names you pass to
` + "`crush models use <large> <small>`" + `. Atoms whose backing provider is
disabled are hidden so the list only shows what you can actually use
right now.

After the atom block, a second section lists every model id from every
enabled provider as a raw "provider/model" string — those are also
accepted by ` + "`crush models use`" + ` as a fallback when a model is not in
the atom registry.

` + "`--json`" + ` emits a structured object: { "atoms": [...], "other_models": [...] }.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		cfg := a.Config()
		if asJSON {
			return emitModelsListJSON(cfg)
		}
		fmt.Print(renderAtomsBlock(cfg))
		fmt.Println()
		fmt.Print(renderOtherModelsBlock(cfg))
		return nil
	},
}

type atomJSON struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Ctx      string   `json:"ctx"`
	Levels   []string `json:"levels,omitempty"`
	Group    string   `json:"group"`
}

type rawModelJSON struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Ctx      int64  `json:"context_window,omitempty"`
	CanReason bool  `json:"can_reason,omitempty"`
}

func emitModelsListJSON(cfg *config.Config) error {
	var atoms []atomJSON
	for _, k := range enabledAtomKeys(cfg) {
		a := atomRegistry[k]
		j := atomJSON{Name: k, Provider: a.Provider, Model: a.Model, Ctx: a.CtxLabel, Group: a.Group}
		if a.EffortSource != nil {
			j.Levels = a.EffortSource.Levels()
		}
		atoms = append(atoms, j)
	}
	var raw []rawModelJSON
	for _, p := range cfg.EnabledProviders() {
		for _, m := range p.Models {
			raw = append(raw, rawModelJSON{
				Provider:  p.ID,
				Model:     m.ID,
				Ctx:       m.ContextWindow,
				CanReason: m.CanReason,
			})
		}
	}
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].Provider != raw[j].Provider {
			return raw[i].Provider < raw[j].Provider
		}
		return raw[i].Model < raw[j].Model
	})
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"atoms":        atoms,
		"other_models": raw,
	})
}

func renderOtherModelsBlock(cfg *config.Config) string {
	enabled := cfg.EnabledProviders()
	if len(enabled) == 0 {
		return "OTHER MODELS: (no enabled providers)\n"
	}
	var b strings.Builder
	b.WriteString("OTHER MODELS (use as `crush models use provider/model[@level] provider/model[@level]`):\n\n")

	byProvider := map[string][]catwalk.Model{}
	providerIDs := make([]string, 0, len(enabled))
	for _, p := range enabled {
		providerIDs = append(providerIDs, p.ID)
		byProvider[p.ID] = p.Models
	}
	sort.Strings(providerIDs)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, pid := range providerIDs {
		models := byProvider[pid]
		if len(models) == 0 {
			fmt.Fprintf(tw, "  %s:\t(no models loaded — run `crush providers update %s`)\n", pid, pid)
			continue
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		for _, m := range models {
			ctx := "?"
			if m.ContextWindow > 0 {
				ctx = humanCtx(m.ContextWindow)
			}
			reason := ""
			if m.CanReason {
				if len(m.ReasoningLevels) > 0 {
					reason = " reason:" + joinLevels(m.ReasoningLevels)
				} else {
					reason = " reasoning"
				}
			}
			fmt.Fprintf(tw, "  %s/%s\t(%s ctx%s)\n", pid, m.ID, ctx, reason)
		}
	}
	tw.Flush()
	return b.String()
}

func humanCtx(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%dk", n/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func joinLevels(ls []string) string {
	out := ""
	for i, l := range ls {
		if i > 0 {
			out += "|"
		}
		out += l
	}
	return out
}

func init() {
	modelsListCmd.Flags().Bool("json", false, "Emit a structured JSON object instead of human-readable text")
	modelsCmd.AddCommand(modelsListCmd)
}
