package cli

import (
	"path/filepath"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

func loadConfigForRepo(repoPath, explicitConfig string) (*config.Config, error) {
	if explicitConfig != "" {
		return config.LoadConfigFromPath(explicitConfig)
	}
	return config.LoadConfig(repoPath)
}

func policyBaseDir(repoPath string, cfg *config.Config) string {
	if repoPath != "" {
		return repoPath
	}
	if cfg != nil && cfg.SourcePath != "" {
		return filepath.Dir(cfg.SourcePath)
	}
	return "."
}
