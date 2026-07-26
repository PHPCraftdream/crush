// Fork addition: shared conversion + filesystem helpers used by the
// `<tool>-init`/`<tool>-del` command family (codex-init/codex-del today;
// gemini-init/gemini-del, grok-init/grok-del, qwen-init/qwen-del follow the
// same pattern). Each of those CLIs has its own on-disk convention for
// "custom command"/"skill" — Claude Code uses `.claude/commands/*.md`
// front-matter, Codex/Grok use Skills-style `<name>/SKILL.md`, Gemini uses
// TOML, Qwen uses a different front-matter placeholder. Rather than
// duplicate the source-of-truth prose four times, we keep ONE canonical
// source per command (claudeSlashCommandTemplate / claudeFallbackCommandTemplate,
// embedded in claude_init.go) and convert it into each tool's native format
// here.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parseSlashCommandSource splits a canonical slash-command .md source
// (YAML front-matter with a single `description:` field, then a Markdown
// body) into its description and body parts. The format is trivial and
// fixed by convention (see claude_slash_command.md / claude_crush_fallback_command.md),
// so this is deliberately plain string parsing rather than a YAML library.
//
// Expected shape:
//
//	---
//	description: <text>
//	---
//
//	<body...>
func parseSlashCommandSource(raw string) (description, body string, err error) {
	lines := strings.Split(raw, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", fmt.Errorf("parseSlashCommandSource: expected source to start with a %q front-matter delimiter", "---")
	}

	const descPrefix = "description: "
	if !strings.HasPrefix(lines[1], descPrefix) {
		return "", "", fmt.Errorf("parseSlashCommandSource: expected line 2 to start with %q, got %q", descPrefix, lines[1])
	}
	description = strings.TrimPrefix(lines[1], descPrefix)

	if len(lines) < 3 || strings.TrimSpace(lines[2]) != "---" {
		return "", "", fmt.Errorf("parseSlashCommandSource: expected a closing %q front-matter delimiter on line 3, got %q", "---", lines[2])
	}

	rest := strings.Join(lines[3:], "\n")
	body = strings.TrimLeft(rest, "\n")
	return description, body, nil
}

// renderFrontMatterMD renders a front-matter-style .md file: a sentinel
// comment, a `description:` front-matter block, then the body with
// `$ARGUMENTS` rewritten to the given placeholder. Used by tools (e.g.
// qwen-init) whose custom-command convention still uses `description:`
// front-matter but a different in-body argument placeholder than Claude
// Code's `$ARGUMENTS`.
func renderFrontMatterMD(sentinelComment, description, body, placeholder string) string {
	return sentinelComment + "\n---\ndescription: " + description + "\n---\n\n" + strings.ReplaceAll(body, "$ARGUMENTS", placeholder)
}

// toGeminiTOML converts a description/body pair into Gemini CLI's custom
// command TOML format. Returns an error if body contains a literal `"""`,
// since that would break the TOML triple-quoted string we emit it into —
// better to fail loudly here than silently corrupt the generated file.
func toGeminiTOML(description, body string) (string, error) {
	if strings.Contains(body, `"""`) {
		return "", fmt.Errorf("toGeminiTOML: body contains a literal %q, which would break the TOML triple-quoted prompt string", `"""`)
	}
	escapedDesc := strings.ReplaceAll(description, `"`, `\"`)
	prompt := strings.ReplaceAll(body, "$ARGUMENTS", "{{args}}")
	return "# crush-slash-command:v1\n" +
		"description = \"" + escapedDesc + "\"\n" +
		"prompt = \"\"\"\n" +
		prompt +
		"\"\"\"\n", nil
}

// toSkillMD converts a description/body pair into Codex/Grok Skills-format
// SKILL.md content. Both tools share the identical Skills convention:
// `<skillsDir>/<name>/SKILL.md` with `name:`/`description:` front-matter.
func toSkillMD(name, description, body string) string {
	return claudeSlashCommandSentinel + "\n" +
		"---\n" +
		"name: " + name + "\n" +
		"description: " + description + "\n" +
		"---\n\n" +
		"Any text you type after invoking this skill is the task — treat it exactly as `$ARGUMENTS` below would have been substituted.\n\n" +
		body
}

// writeSentinelledFile writes content to path, refusing to overwrite a file
// that already exists but doesn't carry our sentinel substring (someone
// else's file with the same name). Creates parent directories as needed.
func writeSentinelledFile(path, sentinelSubstring, content string) error {
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), sentinelSubstring) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// removeSentinelledFile removes path, refusing to delete a file that exists
// but doesn't carry our sentinel substring. Missing file is a no-op.
func removeSentinelledFile(path, sentinelSubstring string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.Contains(string(data), sentinelSubstring) {
		fmt.Fprintf(os.Stderr, "refusing to delete %s — does not look like ours (missing sentinel)\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", path)
	return nil
}

// writeSentinelledSkillDir writes content to <skillsDir>/<name>/SKILL.md,
// following the same sentinel-ownership rule as writeSentinelledFile.
func writeSentinelledSkillDir(skillsDir, name, sentinelSubstring, content string) error {
	return writeSentinelledFile(filepath.Join(skillsDir, name, "SKILL.md"), sentinelSubstring, content)
}

// removeSentinelledSkillDir removes <skillsDir>/<name>/SKILL.md (following
// the same sentinel-ownership rule as removeSentinelledFile), then attempts
// to remove the now-presumably-empty <skillsDir>/<name>/ directory. The
// directory removal error is deliberately ignored: os.Remove on a
// non-empty directory fails and leaves it untouched, which is exactly the
// safe behaviour we want if something else is still using that directory.
func removeSentinelledSkillDir(skillsDir, name, sentinelSubstring string) error {
	dir := filepath.Join(skillsDir, name)
	if err := removeSentinelledFile(filepath.Join(dir, "SKILL.md"), sentinelSubstring); err != nil {
		return err
	}
	_ = os.Remove(dir)
	return nil
}
