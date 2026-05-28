package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func makeItems(n int) []sessionItem {
	items := make([]sessionItem, n)
	for i := range items {
		items[i] = sessionItem{
			id:    fmt.Sprintf("sess-%02d", i),
			hash:  fmt.Sprintf("h%06d", i),
			title: fmt.Sprintf("title %d", i),
		}
	}
	return items
}

func TestTrimSessionItems(t *testing.T) {
	t.Run("under the cap returns input unchanged", func(t *testing.T) {
		in := makeItems(5)
		out, hidden := trimSessionItems(in, 15)
		assert.Len(t, out, 5)
		assert.Equal(t, 0, hidden)
		assert.Equal(t, in[0].id, out[0].id)
	})

	t.Run("exactly at cap returns input unchanged", func(t *testing.T) {
		in := makeItems(15)
		out, hidden := trimSessionItems(in, 15)
		assert.Len(t, out, 15)
		assert.Equal(t, 0, hidden)
	})

	t.Run("over cap trims to max and reports hidden count", func(t *testing.T) {
		in := makeItems(42)
		out, hidden := trimSessionItems(in, 15)
		assert.Len(t, out, 15)
		assert.Equal(t, 27, hidden)
		// First 15 (most recent — list is updated_at DESC) are kept.
		assert.Equal(t, "sess-00", out[0].id)
		assert.Equal(t, "sess-14", out[14].id)
	})

	t.Run("max <= 0 disables the cap", func(t *testing.T) {
		in := makeItems(20)
		out, hidden := trimSessionItems(in, 0)
		assert.Len(t, out, 20)
		assert.Equal(t, 0, hidden)
		out, hidden = trimSessionItems(in, -5)
		assert.Len(t, out, 20)
		assert.Equal(t, 0, hidden)
	})

	t.Run("nil input is safe", func(t *testing.T) {
		out, hidden := trimSessionItems(nil, 15)
		assert.Nil(t, out)
		assert.Equal(t, 0, hidden)
	})

	t.Run("empty input is safe", func(t *testing.T) {
		out, hidden := trimSessionItems([]sessionItem{}, 15)
		assert.Len(t, out, 0)
		assert.Equal(t, 0, hidden)
	})
}

func TestPickerModel_ViewShowsHiddenFooter(t *testing.T) {
	m := pickerModel{items: makeItems(5), hidden: 27}
	view := m.View().Content
	assert.Contains(t, view, "(+27 older sessions not shown")
	assert.Contains(t, view, "sessions list")
}

func TestPickerModel_ViewOmitsFooterWhenNothingHidden(t *testing.T) {
	m := pickerModel{items: makeItems(5), hidden: 0}
	view := m.View().Content
	assert.NotContains(t, view, "not shown")
}

func TestPickerModel_ViewSurfacesHashAndTitle(t *testing.T) {
	// Sanity check that View renders something resembling the picker;
	// not a content-pin, just enough to fail loudly if the View loop
	// regresses to printing nothing.
	m := pickerModel{items: makeItems(3)}
	view := m.View().Content
	for _, item := range m.items {
		assert.True(t, strings.Contains(view, item.hash), "missing hash %q", item.hash)
	}
}
