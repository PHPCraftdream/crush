// Package skills implements the Agent Skills open standard.
// See https://agentskills.io for the specification.
package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/charlievieth/fastwalk"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/pubsub"
	"gopkg.in/yaml.v3"
)

const (
	SkillFileName          = "SKILL.md"
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
)

var (
	namePattern    = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)
	promptReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")

	latestStates   []*SkillState
	latestStatesMu sync.RWMutex
)

// Skill represents a parsed SKILL.md file.
//
// Fork merge note (origin/main 6716ef09 "feat(skills): add support for user
// invocable skills"): added their two new boolean flags (UserInvocable,
// DisableModelInvocation) so SKILL.md files using them stay parseable. The
// rest of their skills-architecture rewrite (Manager, Catalog,
// proto/skills.go) is rejected — it is built around their client-server
// backend model which we deleted. See CHANGELOG.fork.md Section 2.
type Skill struct {
	Name                   string            `yaml:"name" json:"name"`
	Description            string            `yaml:"description" json:"description"`
	UserInvocable          bool              `yaml:"user-invocable" json:"user_invocable"`
	DisableModelInvocation bool              `yaml:"disable-model-invocation" json:"disable_model_invocation"`
	License                string            `yaml:"license,omitempty" json:"license,omitempty"`
	Compatibility          string            `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Metadata               map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Instructions           string            `yaml:"-" json:"instructions"`
	Path                   string            `yaml:"-" json:"path"`
	SkillFilePath          string            `yaml:"-" json:"skill_file_path"`
	// Source identifies which AI tool this skill/command comes from (e.g.
	// "claude", "gemini", "crush"). Fork-only field — drives our
	// DiscoverCommands scrape of ~/.claude/commands/, ~/.gemini/commands/,
	// ~/.crush/commands/ etc that the WebUI surfaces as slash-commands.
	Source  string `yaml:"-" json:"source,omitempty"`
	Builtin bool   `yaml:"-" json:"-"`
}

// CommandDir is a directory to scan for simple markdown command files.
type CommandDir struct {
	Path   string
	Source string
}

// DefaultCommandDirs returns directories from popular AI coding tools to scan
// for simple markdown command/prompt files.
func DefaultCommandDirs() []CommandDir {
	h := home.Dir()
	dirs := []CommandDir{
		{Path: filepath.Join(h, ".claude", "commands"), Source: "claude"},
		{Path: filepath.Join(h, ".gemini", "commands"), Source: "gemini"},
		{Path: filepath.Join(h, ".qwen", "commands"), Source: "qwen"},
		{Path: filepath.Join(h, ".cursor", "rules"), Source: "cursor"},
		{Path: filepath.Join(h, ".zed", "prompts"), Source: "zed"},
		{Path: filepath.Join(h, ".windsurf", "commands"), Source: "windsurf"},
	}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs,
			CommandDir{Path: filepath.Join(cwd, ".claude", "commands"), Source: "claude"},
			CommandDir{Path: filepath.Join(cwd, ".gemini", "commands"), Source: "gemini"},
			CommandDir{Path: filepath.Join(cwd, ".qwen", "commands"), Source: "qwen"},
			CommandDir{Path: filepath.Join(cwd, ".crush", "commands"), Source: "crush"},
		)
	}
	return dirs
}

func SourceFromPath(path string) string {
	norm := filepath.ToSlash(strings.ToLower(path))
	switch {
	case strings.Contains(norm, "/.claude/") || strings.Contains(norm, "/.claude\\"):
		return "claude"
	case strings.Contains(norm, "/.gemini/") || strings.Contains(norm, "/.gemini\\"):
		return "gemini"
	case strings.Contains(norm, "/.qwen/") || strings.Contains(norm, "/.qwen\\"):
		return "qwen"
	case strings.Contains(norm, "/.cursor/") || strings.Contains(norm, "/.cursor\\"):
		return "cursor"
	case strings.Contains(norm, "/.zed/") || strings.Contains(norm, "/.zed\\"):
		return "zed"
	case strings.Contains(norm, "/.windsurf/") || strings.Contains(norm, "/.windsurf\\"):
		return "windsurf"
	case strings.Contains(norm, "crush"):
		return "crush"
	default:
		return "local"
	}
}

// ErrPerModelCommand is returned by ParseCommand when the command file pins
// a specific model to run under (a `model:` frontmatter field — the pattern
// `cah install` uses for its per-model shortcuts like /oxx, /sh, /fl, ...).
// Those are meant to be typed directly by the operator to switch the
// current turn's model, not surfaced in crush's own command/skill list —
// crush has its own model-selection UI and doesn't route slash-commands to
// specific models.
var ErrPerModelCommand = errors.New("per-model command, not a general-purpose command")

func ParseCommand(path, source string) (*Skill, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	filename := strings.TrimSuffix(filepath.Base(path), ".md")
	name := filename
	description := ""
	instructions := strings.TrimSpace(string(content))

	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	if strings.HasPrefix(normalized, "---\n") {
		if fm, body, ferr := splitFrontmatter(normalized); ferr == nil {
			var meta struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
				Model       string `yaml:"model"`
			}
			if yaml.Unmarshal([]byte(fm), &meta) == nil {
				if meta.Model != "" {
					return nil, ErrPerModelCommand
				}
				if meta.Name != "" {
					name = meta.Name
				}
				if meta.Description != "" {
					description = meta.Description
				}
			}
			instructions = strings.TrimSpace(body)
		}
	}

	if description == "" {
		for _, line := range strings.Split(instructions, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "#") {
				description = strings.TrimSpace(strings.TrimLeft(line, "# "))
			} else {
				description = line
				if len(description) > 200 {
					description = description[:197] + "..."
				}
			}
			break
		}
	}
	if description == "" {
		description = name
	}

	return &Skill{
		Name:          name,
		Description:   description,
		Instructions:  instructions,
		Path:          filepath.Dir(path),
		SkillFilePath: path,
		Source:        source,
	}, nil
}

func DiscoverCommands(dirs []CommandDir) []*Skill {
	var result []*Skill
	seen := make(map[string]bool)

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				continue
			}
			path := filepath.Join(dir.Path, e.Name())
			if seen[path] {
				continue
			}
			seen[path] = true

			skill, err := ParseCommand(path, dir.Source)
			if errors.Is(err, ErrPerModelCommand) {
				continue
			}
			if err != nil {
				slog.Warn("Failed to parse command file", "path", path, "error", err)
				continue
			}
			result = append(result, skill)
		}
	}
	return result
}

// DiscoveryState represents the outcome of discovering a single skill file.
type DiscoveryState int

const (
	StateNormal DiscoveryState = iota
	StateError
)

type SkillState struct {
	Name  string
	Path  string
	State DiscoveryState
	Err   error
}

type Event struct {
	States []*SkillState
}

var broker = pubsub.NewBroker[Event]()

func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return broker.Subscribe(ctx)
}

// PublishStates publishes a skill discovery event with the given states.
func PublishStates(states []*SkillState) {
	broker.Publish(pubsub.UpdatedEvent, Event{States: cloneStates(states)})
}

// cloneStates returns a deep copy of the given state slice so callers cannot
// accidentally mutate the source.
func cloneStates(states []*SkillState) []*SkillState {
	if states == nil {
		return nil
	}
	result := make([]*SkillState, len(states))
	for i, s := range states {
		clone := *s
		result[i] = &clone
	}
	return result
}

// GetLatestStates returns the latest discovery states.
func GetLatestStates() []*SkillState {
	latestStatesMu.RLock()
	defer latestStatesMu.RUnlock()
	return cloneStates(latestStates)
}

// SetLatestStates stores the given states in the package-level cache so that
// GetLatestStates can return them synchronously before the first pubsub event
// arrives.
func SetLatestStates(states []*SkillState) {
	latestStatesMu.Lock()
	latestStates = cloneStates(states)
	latestStatesMu.Unlock()
}

// Validate checks if the skill meets spec requirements.
func (s *Skill) Validate() error {
	var errs []error

	if s.Name == "" {
		errs = append(errs, errors.New("name is required"))
	} else {
		if len(s.Name) > MaxNameLength {
			errs = append(errs, fmt.Errorf("name exceeds %d characters", MaxNameLength))
		}
		if !namePattern.MatchString(s.Name) {
			errs = append(errs, errors.New("name must be alphanumeric with hyphens, no leading/trailing/consecutive hyphens"))
		}
		if s.Path != "" && !strings.EqualFold(filepath.Base(s.Path), s.Name) {
			errs = append(errs, fmt.Errorf("name %q must match directory %q", s.Name, filepath.Base(s.Path)))
		}
	}

	if s.Description == "" {
		errs = append(errs, errors.New("description is required"))
	} else if len(s.Description) > MaxDescriptionLength {
		errs = append(errs, fmt.Errorf("description exceeds %d characters", MaxDescriptionLength))
	}

	if len(s.Compatibility) > MaxCompatibilityLength {
		errs = append(errs, fmt.Errorf("compatibility exceeds %d characters", MaxCompatibilityLength))
	}

	return errors.Join(errs...)
}

// Parse parses a SKILL.md file from disk.
func Parse(path string) (*Skill, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	skill, err := ParseContent(content)
	if err != nil {
		return nil, err
	}

	skill.Path = filepath.Dir(path)
	skill.SkillFilePath = path

	return skill, nil
}

// ParseContent parses a SKILL.md from raw bytes.
func ParseContent(content []byte) (*Skill, error) {
	frontmatter, body, err := splitFrontmatter(string(content))
	if err != nil {
		return nil, err
	}

	var skill Skill
	if err := yaml.Unmarshal([]byte(frontmatter), &skill); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}

	skill.Instructions = strings.TrimSpace(body)

	return &skill, nil
}

// splitFrontmatter extracts YAML frontmatter and body from markdown content.
func splitFrontmatter(content string) (frontmatter, body string, err error) {
	// Strip UTF-8 BOM for compatibility with editors that include it.
	content = strings.TrimPrefix(content, "\uFEFF")
	// Normalize line endings to \n for consistent parsing.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != ""
	})
	if start == -1 || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("no YAML frontmatter found")
	}

	endOffset := slices.IndexFunc(lines[start+1:], func(line string) bool {
		return strings.TrimSpace(line) == "---"
	})
	if endOffset == -1 {
		return "", "", errors.New("unclosed frontmatter")
	}
	end := start + 1 + endOffset

	frontmatter = strings.Join(lines[start+1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	return frontmatter, body, nil
}

// Discover finds all valid skills in the given paths.
func Discover(paths []string) []*Skill {
	skills, _ := DiscoverWithStates(paths)
	return skills
}

// DiscoverWithStates finds all valid skills in the given paths and also
// returns a per-file state slice describing parse/validation outcomes. Useful
// for diagnostics and UI reporting.
func DiscoverWithStates(paths []string) ([]*Skill, []*SkillState) {
	var skills []*Skill
	var states []*SkillState
	var mu sync.Mutex
	seen := make(map[string]bool)
	addState := func(name, path string, state DiscoveryState, err error) {
		mu.Lock()
		states = append(states, &SkillState{
			Name:  name,
			Path:  path,
			State: state,
			Err:   err,
		})
		mu.Unlock()
	}

	for _, base := range paths {
		// We use fastwalk with Follow: true instead of filepath.WalkDir because
		// WalkDir doesn't follow symlinked directories at any depth—only entry
		// points. This ensures skills in symlinked subdirectories are discovered.
		// fastwalk is concurrent, so we protect shared state (seen, skills) with mu.
		conf := fastwalk.Config{
			Follow:  true,
			ToSlash: fastwalk.DefaultToSlash(),
		}
		err := fastwalk.Walk(&conf, base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				slog.Warn("Failed to walk skills path entry", "base", base, "path", path, "error", err)
				addState("", path, StateError, err)
				return nil
			}
			if d.IsDir() || d.Name() != SkillFileName {
				return nil
			}
			mu.Lock()
			if seen[path] {
				mu.Unlock()
				return nil
			}
			seen[path] = true
			mu.Unlock()
			skill, err := Parse(path)
			if err != nil {
				slog.Warn("Failed to parse skill file", "path", path, "error", err)
				addState("", path, StateError, err)
				return nil
			}
			if err := skill.Validate(); err != nil {
				slog.Warn("Skill validation failed", "path", path, "error", err)
				addState(skill.Name, path, StateError, err)
				return nil
			}
			skill.Source = SourceFromPath(path)
			slog.Debug("Successfully loaded skill", "name", skill.Name, "path", path)
			mu.Lock()
			skills = append(skills, skill)
			mu.Unlock()
			addState(skill.Name, path, StateNormal, nil)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to walk skills path", "path", base, "error", err)
		}
	}

	// fastwalk traversal order is non-deterministic, so sort for stable output.
	// Sort by path first, then alphabetically by name within each path.
	slices.SortStableFunc(skills, func(a, b *Skill) int {
		if c := strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)); c != 0 {
			return c
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	return skills, states
}

// ToPromptXML generates XML for injection into the system prompt.
// Skills with DisableModelInvocation set to true are excluded.
func ToPromptXML(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<available_skills>\n")
	for _, s := range skills {
		// Skip skills that have disable-model-invocation set
		if s.DisableModelInvocation {
			continue
		}
		sb.WriteString("  <skill>\n")
		fmt.Fprintf(&sb, "    <name>%s</name>\n", escape(s.Name))
		fmt.Fprintf(&sb, "    <description>%s</description>\n", escape(s.Description))
		fmt.Fprintf(&sb, "    <location>%s</location>\n", escape(s.SkillFilePath))
		if s.Builtin {
			sb.WriteString("    <type>builtin</type>\n")
		}
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</available_skills>")
	return sb.String()
}

// FormatInvocation generates XML for a skill when invoked as a user command.
func (s *Skill) FormatInvocation() string {
	var sb strings.Builder
	sb.WriteString("<loaded_skill>\n")
	fmt.Fprintf(&sb, "  <name>%s</name>\n", escape(s.Name))
	fmt.Fprintf(&sb, "  <description>%s</description>\n", escape(s.Description))
	fmt.Fprintf(&sb, "  <location>%s</location>\n", escape(s.SkillFilePath))
	sb.WriteString("  <instructions>\n")
	sb.WriteString(escape(s.Instructions))
	sb.WriteString("\n  </instructions>\n")
	sb.WriteString("</loaded_skill>")
	return sb.String()
}

func escape(s string) string {
	return promptReplacer.Replace(s)
}

// DeduplicateStates removes duplicate skill states by name. When duplicates exist,
// the last occurrence wins (consistent with Deduplicate for skills).
func DeduplicateStates(all []*SkillState) []*SkillState {
	seen := make(map[string]int, len(all))
	for i, s := range all {
		if s.Name != "" {
			seen[s.Name] = i
		}
	}

	result := make([]*SkillState, 0, len(seen))
	for i, s := range all {
		// If it's the last occurrence of this name, or it has no name (error state), keep it
		if s.Name == "" || seen[s.Name] == i {
			result = append(result, s)
		}
	}
	return result
}

// Deduplicate removes duplicate skills by name. When duplicates exist, the
// last occurrence wins. This means user skills (appended after builtins)
// override builtin skills with the same name.
func Deduplicate(all []*Skill) []*Skill {
	seen := make(map[string]int, len(all))
	for i, s := range all {
		seen[s.Name] = i
	}

	result := make([]*Skill, 0, len(seen))
	for i, s := range all {
		if seen[s.Name] == i {
			result = append(result, s)
		}
	}
	return result
}

// ApproxTokenCount returns a rough estimate of how many tokens a string
// occupies when sent to an LLM. Uses the common ~4-chars-per-token heuristic
// that approximates GPT/Claude tokenizers well enough for diagnostic logging.
func ApproxTokenCount(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// Filter removes skills whose names appear in the disabled list.
func Filter(all []*Skill, disabled []string) []*Skill {
	if len(disabled) == 0 {
		return all
	}

	disabledSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disabledSet[name] = true
	}

	result := make([]*Skill, 0, len(all))
	for _, s := range all {
		if !disabledSet[s.Name] {
			result = append(result, s)
		}
	}
	return result
}
