package prompt

import "testing"

func TestDedupeContextFiles_DropsIdenticalContent(t *testing.T) {
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "Follow the style guide.\n"},
		{Path: "CLAUDE.md", Content: "Follow the style guide.\n"},
		{Path: "GEMINI.md", Content: "Follow the style guide.\n"},
	}

	got := dedupeContextFiles(files)

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1; got = %+v", len(got), got)
	}
	if got[0].Path != "AGENTS.md" {
		t.Errorf("kept path = %q, want %q (first occurrence)", got[0].Path, "AGENTS.md")
	}
}

func TestDedupeContextFiles_KeepsDifferentContent(t *testing.T) {
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "Use tabs.\n"},
		{Path: "CLAUDE.md", Content: "Use spaces.\n"},
	}

	got := dedupeContextFiles(files)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got = %+v", len(got), got)
	}
}

func TestDedupeContextFiles_EmptyInput(t *testing.T) {
	got := dedupeContextFiles(nil)
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

func TestDedupeContextFiles_MixOfDuplicateAndUniqueContent(t *testing.T) {
	files := []ContextFile{
		{Path: "AGENTS.md", Content: "shared\n"},
		{Path: "notes.md", Content: "unique\n"},
		{Path: "CLAUDE.md", Content: "shared\n"},
		{Path: "GEMINI.md", Content: "shared\n"},
	}

	got := dedupeContextFiles(files)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got = %+v", len(got), got)
	}
	paths := map[string]bool{got[0].Path: true, got[1].Path: true}
	if !paths["AGENTS.md"] || !paths["notes.md"] {
		t.Errorf("expected AGENTS.md and notes.md to survive, got paths %+v", got)
	}
}

func TestFlattenContextFiles_SortsByPathKeyDeterministically(t *testing.T) {
	byPath := map[string][]ContextFile{
		"z.md": {{Path: "z.md", Content: "z"}},
		"a.md": {{Path: "a.md", Content: "a"}},
		"m.md": {{Path: "m.md", Content: "m"}},
	}

	got := flattenContextFiles(byPath)

	want := []string{"a.md", "m.md", "z.md"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("got[%d].Path = %q, want %q", i, got[i].Path, w)
		}
	}
}

func TestFlattenContextFiles_PreservesMultipleFilesPerPathKey(t *testing.T) {
	byPath := map[string][]ContextFile{
		".cursor/rules/": {
			{Path: ".cursor/rules/a.md", Content: "a"},
			{Path: ".cursor/rules/b.md", Content: "b"},
		},
	}

	got := flattenContextFiles(byPath)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; got = %+v", len(got), got)
	}
}
