// Package skills implements the Agent Skills open standard.
// See https://agentskills.io for the specification.
package skills

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/home"
	"github.com/charlievieth/fastwalk"
	"gopkg.in/yaml.v3"
)

const (
	SkillFileName          = "SKILL.md"
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

// Skill represents a parsed SKILL.md file.
type Skill struct {
	Name          string            `yaml:"name" json:"name"`
	Description   string            `yaml:"description" json:"description"`
	License       string            `yaml:"license,omitempty" json:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Instructions  string            `yaml:"-" json:"instructions"`
	Path          string            `yaml:"-" json:"path"`
	SkillFilePath string            `yaml:"-" json:"skill_file_path"`
	// Source identifies which AI tool this skill/command comes from (e.g. "claude", "gemini", "crush").
	Source string `yaml:"-" json:"source,omitempty"`
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
	// Project-local command directories
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

// SourceFromPath derives the source label from a skill or command file path.
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

// ParseCommand parses a simple markdown file as a slash command.
// Unlike SKILL.md files, these don't require YAML frontmatter — the filename
// becomes the command name and the first heading/paragraph is the description.
func ParseCommand(path, source string) (*Skill, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	filename := strings.TrimSuffix(filepath.Base(path), ".md")
	name := filename
	description := ""
	instructions := strings.TrimSpace(string(content))

	// Try to extract YAML frontmatter if present
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	if strings.HasPrefix(normalized, "---\n") {
		if fm, body, ferr := splitFrontmatter(normalized); ferr == nil {
			var meta struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
			}
			if yaml.Unmarshal([]byte(fm), &meta) == nil {
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

	// Extract description from the first heading or paragraph
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

// DiscoverCommands scans directories for simple markdown command files.
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
			if err != nil {
				slog.Warn("Failed to parse command file", "path", path, "error", err)
				continue
			}
			result = append(result, skill)
		}
	}
	return result
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

// Parse parses a SKILL.md file.
func Parse(path string) (*Skill, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	frontmatter, body, err := splitFrontmatter(string(content))
	if err != nil {
		return nil, err
	}

	var skill Skill
	if err := yaml.Unmarshal([]byte(frontmatter), &skill); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}

	skill.Instructions = strings.TrimSpace(body)
	skill.Path = filepath.Dir(path)
	skill.SkillFilePath = path

	return &skill, nil
}

// splitFrontmatter extracts YAML frontmatter and body from markdown content.
func splitFrontmatter(content string) (frontmatter, body string, err error) {
	// Normalize line endings to \n for consistent parsing.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return "", "", errors.New("no YAML frontmatter found")
	}

	rest := strings.TrimPrefix(content, "---\n")
	before, after, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", "", errors.New("unclosed frontmatter")
	}

	return before, after, nil
}

// Discover finds all valid skills in the given paths.
func Discover(paths []string) []*Skill {
	var skills []*Skill
	var mu sync.Mutex
	seen := make(map[string]bool)

	for _, base := range paths {
		// We use fastwalk with Follow: true instead of filepath.WalkDir because
		// WalkDir doesn't follow symlinked directories at any depth—only entry
		// points. This ensures skills in symlinked subdirectories are discovered.
		// fastwalk is concurrent, so we protect shared state (seen, skills) with mu.
		conf := fastwalk.Config{
			Follow:  true,
			ToSlash: fastwalk.DefaultToSlash(),
		}
		fastwalk.Walk(&conf, base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
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
				return nil
			}
			if err := skill.Validate(); err != nil {
				slog.Warn("Skill validation failed", "path", path, "error", err)
				return nil
			}
			skill.Source = SourceFromPath(path)
			slog.Debug("Successfully loaded skill", "name", skill.Name, "path", path)
			mu.Lock()
			skills = append(skills, skill)
			mu.Unlock()
			return nil
		})
	}

	return skills
}

// ToPromptXML generates XML for injection into the system prompt.
func ToPromptXML(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<available_skills>\n")
	for _, s := range skills {
		sb.WriteString("  <skill>\n")
		fmt.Fprintf(&sb, "    <name>%s</name>\n", escape(s.Name))
		fmt.Fprintf(&sb, "    <description>%s</description>\n", escape(s.Description))
		fmt.Fprintf(&sb, "    <location>%s</location>\n", escape(s.SkillFilePath))
		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</available_skills>")
	return sb.String()
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}
