package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMailbox_PopAllSubmitted verifies that popAllSubmitted returns all
// queued entries and clears the submitted queue. This is a helper for
// abandonOwnershipWithHandoff.
func TestMailbox_PopAllSubmitted(t *testing.T) {
	t.Parallel()

	mb := &mailbox{
		state: mbIdle,
		submitted: []SessionAgentCall{
			{SessionID: "s1", Prompt: "first"},
			{SessionID: "s1", Prompt: "second"},
			{SessionID: "s1", Prompt: "third"},
		},
	}

	all := mb.popAllSubmitted()
	require.Len(t, all, 3, "must return all three entries")
	require.Equal(t, "first", all[0].Prompt)
	require.Equal(t, "second", all[1].Prompt)
	require.Equal(t, "third", all[2].Prompt)

	// Verify the queue is cleared.
	require.Len(t, mb.submitted, 0, "submitted must be cleared")
}

// TestMailbox_PopAllSubmitted_EmptyReturnsNil verifies that popAllSubmitted
// returns nil (not an empty slice) when the queue is empty.
func TestMailbox_PopAllSubmitted_EmptyReturnsNil(t *testing.T) {
	t.Parallel()

	mb := &mailbox{
		state:     mbIdle,
		submitted: []SessionAgentCall{},
	}

	all := mb.popAllSubmitted()
	require.Nil(t, all, "must return nil for empty queue")
	require.Len(t, mb.submitted, 0)
}

// TestMailbox_PopAllSubmitted_DoesNotClearReplacement verifies that
// popAllSubmitted only clears submitted, not replacement. The caller
// (abandonOwnership) folds replacement into submitted before calling
// popAllSubmitted, so replacement should be empty in production use.
func TestMailbox_PopAllSubmitted_DoesNotClearReplacement(t *testing.T) {
	t.Parallel()

	replacementCall := SessionAgentCall{SessionID: "s1", Prompt: "replacement"}
	mb := &mailbox{
		state:       mbIdle,
		submitted:   []SessionAgentCall{{SessionID: "s1", Prompt: "queued"}},
		replacement: &replacementCall,
	}

	all := mb.popAllSubmitted()
	require.Len(t, all, 1, "must return only submitted entries")
	require.Equal(t, "queued", all[0].Prompt)

	// Replacement must still be there.
	require.NotNil(t, mb.replacement, "replacement must not be cleared")
	require.Equal(t, "replacement", mb.replacement.Prompt)
}
