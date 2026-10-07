package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

func aiConfigForCommand(cmd *cobra.Command, cfg *config.Config) *config.AIConfig {
	var out config.AIConfig
	if cfg != nil && cfg.AI != nil {
		out = *cfg.AI
		out.AllowedClassifications = append([]string(nil), cfg.AI.AllowedClassifications...)
	}
	if flagChanged(cmd, "ai-assessment") {
		out.Enabled = config.BoolPtr(aiAssessment)
	}
	if flagChanged(cmd, "ai-provider") {
		out.Provider = strings.TrimSpace(aiProvider)
	}
	if flagChanged(cmd, "ai-model") {
		out.Model = strings.TrimSpace(aiModel)
	}
	if flagChanged(cmd, "ai-fail-on-error") {
		out.FailOnError = config.BoolPtr(aiFailOnError)
	}
	if out.Enabled == nil {
		return nil
	}
	return &out
}

func aiConfigForAction(reqAIEnabled string, reqProvider string, reqModel string, reqFailOnError string, cfg *config.Config) *config.AIConfig {
	var out config.AIConfig
	if cfg != nil && cfg.AI != nil {
		out = *cfg.AI
		out.AllowedClassifications = append([]string(nil), cfg.AI.AllowedClassifications...)
	}
	switch strings.ToLower(strings.TrimSpace(reqAIEnabled)) {
	case "true", "1", "yes", "on":
		out.Enabled = config.BoolPtr(true)
	case "false", "0", "no", "off":
		out.Enabled = config.BoolPtr(false)
	}
	if reqProvider != "" {
		out.Provider = strings.TrimSpace(reqProvider)
	}
	if reqModel != "" {
		out.Model = strings.TrimSpace(reqModel)
	}
	switch strings.ToLower(strings.TrimSpace(reqFailOnError)) {
	case "true", "1", "yes", "on":
		out.FailOnError = config.BoolPtr(true)
	case "false", "0", "no", "off":
		out.FailOnError = config.BoolPtr(false)
	}
	if out.Enabled == nil {
		return nil
	}
	return &out
}
