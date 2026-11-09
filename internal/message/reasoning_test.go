package message

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"
)

// OnReasoningEnd in internal/agent/agent.go stores the provider's
// reasoning metadata and then calls FinishThinking; what it stored must
// still be there for the next request.
func TestFinishThinkingKeepsProviderReasoningData(t *testing.T) {
	t.Parallel()

	t.Run("google thought signature", func(t *testing.T) {
		t.Parallel()

		msg := Message{Role: Assistant}
		msg.AppendReasoningContent("thinking")
		msg.AppendThoughtSignature("sig", "call_1")
		msg.FinishThinking()

		reasoning := msg.ReasoningContent()
		require.Equal(t, "sig", reasoning.ThoughtSignature)
		require.Equal(t, "call_1", reasoning.ToolID)
		require.NotZero(t, reasoning.FinishedAt)

		ai := msg.ToAIMessage()
		require.Len(t, ai, 1)
		part, ok := ai[0].Content[0].(fantasy.ReasoningPart)
		require.True(t, ok)
		require.Equal(t, &google.ReasoningMetadata{Signature: "sig", ToolID: "call_1"}, part.ProviderOptions[google.Name])
	})

	t.Run("openai responses data", func(t *testing.T) {
		t.Parallel()

		data := &openai.ResponsesReasoningMetadata{ItemID: "rs_1"}
		msg := Message{Role: Assistant}
		msg.AppendReasoningContent("thinking")
		msg.SetReasoningResponsesData(data)
		msg.FinishThinking()

		reasoning := msg.ReasoningContent()
		require.Same(t, data, reasoning.ResponsesData)
		require.NotZero(t, reasoning.FinishedAt)

		ai := msg.ToAIMessage()
		require.Len(t, ai, 1)
		part, ok := ai[0].Content[0].(fantasy.ReasoningPart)
		require.True(t, ok)
		require.Equal(t, data, part.ProviderOptions[openai.Name])
	})
}
