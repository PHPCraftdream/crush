package agent

import (
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

// toSessionModelCfg converts config.SelectedModel to session.ModelCfg.
// This is needed because session.ModelCfg is a mirror of config.SelectedModel
// without importing the config package (to avoid import cycles).
func toSessionModelCfg(cfg config.SelectedModel) session.ModelCfg {
	return session.ModelCfg{
		Model:            cfg.Model,
		Provider:         cfg.Provider,
		ReasoningEffort:  cfg.ReasoningEffort,
		Think:            cfg.Think,
		MaxTokens:        cfg.MaxTokens,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		TopK:             cfg.TopK,
		FrequencyPenalty: cfg.FrequencyPenalty,
		PresencePenalty:  cfg.PresencePenalty,
		ProviderOptions:  cfg.ProviderOptions,
	}
}

// fromSessionModelCfg converts session.ModelCfg to config.SelectedModel.
// This is the inverse of toSessionModelCfg.
func fromSessionModelCfg(cfg session.ModelCfg) config.SelectedModel {
	return config.SelectedModel{
		Model:            cfg.Model,
		Provider:         cfg.Provider,
		ReasoningEffort:  cfg.ReasoningEffort,
		Think:            cfg.Think,
		MaxTokens:        cfg.MaxTokens,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		TopK:             cfg.TopK,
		FrequencyPenalty: cfg.FrequencyPenalty,
		PresencePenalty:  cfg.PresencePenalty,
		ProviderOptions:  cfg.ProviderOptions,
	}
}

// ToSessionAgentCallData converts agent.SessionAgentCall to session.SessionAgentCallData
// for durable queue serialization (task #340, ROUND 3 migration).
//
// We only serialize ModelCfg because:
//   - ModelCfg is the per-session pinned snapshot (task #265, P0-1)
//   - ProviderOptions, Temperature, TopP, TopK, FrequencyPenalty, PresencePenalty
//     are pure functions of (Model, ProviderConfig) computed via mergeCallOptions
//     during pump execution ( ROUND 3 design decision).
//   - The live fantasy.LanguageModel and CatwalkCfg in Model are NOT serializable
//     and will be reconstructed by coordinator.RebuildSessionAgentCall.
//
// LogicalCallID is serialized to ensure the stable idempotency key survives
// the durable serialization boundary (P2-1 fix, P0-1 release blocker).
func ToSessionAgentCallData(call SessionAgentCall) session.SessionAgentCallData {
	var largeModel, smallModel *session.ModelCfg
	if call.LargeModel != nil {
		cfg := toSessionModelCfg(call.LargeModel.ModelCfg)
		largeModel = &cfg
	}
	if call.SmallModel != nil {
		cfg := toSessionModelCfg(call.SmallModel.ModelCfg)
		smallModel = &cfg
	}

	return session.SessionAgentCallData{
		SessionID:            call.SessionID,
		LogicalCallID:        call.LogicalCallID,
		Prompt:               call.Prompt,
		Attachments:          call.Attachments,
		MaxOutputTokens:      call.MaxOutputTokens,
		NonInteractive:       call.NonInteractive,
		SystemPromptOverride: call.SystemPromptOverride,
		MaxCost:              call.MaxCost,
		MaxTokens:            call.MaxTokens,
		ExistingMessageID:    call.ExistingMessageID,
		InjectID:             call.InjectID,
		LargeModel:           largeModel,
		SmallModel:           smallModel,
		SystemPromptPrefix:   call.SystemPromptPrefix,
		SystemPrompt:         call.SystemPrompt,
	}
}

// FromSessionAgentCallData converts session.SessionAgentCallData back to agent.SessionAgentCall
// for durable queue deserialization (task #340, ROUND 3 migration).
//
// This function only converts the data that was serialized. The full Model
// (with live fantasy.LanguageModel and CatwalkCfg) will be reconstructed by
// coordinator.RebuildSessionAgentCall.
//
// LogicalCallID is restored to ensure the stable idempotency key survives
// the durable serialization boundary (P2-1 fix, P0-1 release blocker).
func FromSessionAgentCallData(callData session.SessionAgentCallData) SessionAgentCall {
	return SessionAgentCall{
		SessionID:            callData.SessionID,
		LogicalCallID:        callData.LogicalCallID,
		Prompt:               callData.Prompt,
		Attachments:          callData.Attachments,
		MaxOutputTokens:      callData.MaxOutputTokens,
		NonInteractive:       callData.NonInteractive,
		SystemPromptOverride: callData.SystemPromptOverride,
		MaxCost:              callData.MaxCost,
		MaxTokens:            callData.MaxTokens,
		ExistingMessageID:    callData.ExistingMessageID,
		InjectID:             callData.InjectID,
		SystemPromptPrefix:   callData.SystemPromptPrefix,
		SystemPrompt:         callData.SystemPrompt,
		// LargeModel and SmallModel are NOT set here — they will be reconstructed
		// by coordinator.RebuildSessionAgentCall.
	}
}
