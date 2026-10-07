package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/filter"
	"gopkg.in/yaml.v3"
)

var searchPaths = []string{
	".gitops-preview.yaml",
	".gitops-preview.yml",
	".fmp.yaml",
	".fmp.yml",
	".github/fmp.yaml",
}

type Config struct {
	Paths        []string                 `yaml:"paths,omitempty"`
	Clusters     map[string]ClusterConfig `yaml:"clusters,omitempty"`
	Recursive    *bool                    `yaml:"recursive,omitempty"`
	Helm         *bool                    `yaml:"helm,omitempty"`
	ResolveGit   *bool                    `yaml:"resolve-git,omitempty"`
	SOPSDecrypt  *bool                    `yaml:"sops-decrypt,omitempty"`
	Sort         *bool                    `yaml:"sort,omitempty"`
	ExcludeCRDs  *bool                    `yaml:"exclude-crds,omitempty"`
	Filters      filter.FilterConfig      `yaml:",inline"`
	HelmSettings *HelmSettings            `yaml:"helm-settings,omitempty"`
	Crossplane   *CrossplaneSettings      `yaml:"crossplane,omitempty"`
	Policies     *PolicyConfig            `yaml:"policies,omitempty"`
	AI           *AIConfig                `yaml:"ai,omitempty"`
	SourcePath   string                   `yaml:"-"`
}

// ClusterConfig holds one or more paths for a named cluster.
type ClusterConfig struct {
	Paths []string
}

// UnmarshalYAML supports both a single string and a list of strings.
func (c *ClusterConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var path string
		if err := node.Decode(&path); err != nil {
			return err
		}
		c.Paths = []string{path}
		return nil
	case yaml.SequenceNode:
		return node.Decode(&c.Paths)
	default:
		return fmt.Errorf("cluster config must be a string or list of strings")
	}
}

type HelmSettings struct {
	RegistryConfig   string `yaml:"registry-config,omitempty" json:"registryConfig,omitempty"`
	RepositoryConfig string `yaml:"repository-config,omitempty" json:"repositoryConfig,omitempty"`
	RepositoryCache  string `yaml:"repository-cache,omitempty" json:"repositoryCache,omitempty"`
}

// CrossplaneSettings contains repository preferences, never execution grants.
// Executables and development endpoints must come from trusted CLI invocation.
type CrossplaneSettings struct {
	Enabled      *bool  `yaml:"enabled,omitempty"`
	Timeout      string `yaml:"timeout,omitempty"`
	MaxFunctions int    `yaml:"max-functions,omitempty"`
}

type PolicyConfig struct {
	Builtin []string             `yaml:"builtin,omitempty"`
	Modules []string             `yaml:"modules,omitempty"`
	Inline  []string             `yaml:"inline,omitempty"`
	FailOn  []string             `yaml:"fail-on,omitempty"`
	Labels  map[string]LabelList `yaml:"labels,omitempty"`
}

type AIConfig struct {
	Enabled                 *bool    `yaml:"enabled,omitempty"`
	Provider                string   `yaml:"provider,omitempty"`
	Model                   string   `yaml:"model,omitempty"`
	FailOnError             *bool    `yaml:"fail-on-error,omitempty"`
	Instructions            string   `yaml:"instructions,omitempty"`
	AllowedClassifications  []string `yaml:"allowed-classifications,omitempty"`
	MaxInputBytes           int      `yaml:"max-input-bytes,omitempty"`
	MaxDiffLinesPerResource int      `yaml:"max-diff-lines-per-resource,omitempty"`
	Timeout                 string   `yaml:"timeout,omitempty"`
}

type LabelList []string

func (l *LabelList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var label string
		if err := value.Decode(&label); err != nil {
			return err
		}
		if label == "" {
			*l = nil
			return nil
		}
		*l = []string{label}
		return nil
	case yaml.SequenceNode:
		var labels []string
		if err := value.Decode(&labels); err != nil {
			return err
		}
		*l = labels
		return nil
	case 0:
		*l = nil
		return nil
	default:
		return fmt.Errorf("labels entries must be a string or list of strings")
	}
}

func DiscoverConfigPath(repoPath string) string {
	for _, p := range searchPaths {
		full := filepath.Join(repoPath, p)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return ""
}

func LoadConfig(repoPath string) (*Config, error) {
	configPath := DiscoverConfigPath(repoPath)
	if configPath == "" {
		return nil, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", configPath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", configPath, err)
	}
	cfg.SourcePath = configPath

	return &cfg, nil
}

func LoadConfigFromPath(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", configPath, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", configPath, err)
	}
	cfg.SourcePath = configPath

	return &cfg, nil
}

func BoolPtr(b bool) *bool {
	return &b
}

func BoolOr(ptr *bool, def bool) bool {
	if ptr == nil {
		return def
	}
	return *ptr
}

// ParseClusterPath splits a path string into cluster and path components.
// If the string contains a colon, the part before the first colon is the cluster
// and the part after is the path. Otherwise cluster is empty.
func ParseClusterPath(s string) (cluster, path string) {
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// ClusterPaths normalizes both prefixed Paths and the Clusters map into a
// map of cluster name → list of paths.
//
// If no prefixes are present in Paths and Clusters is empty, it returns nil
// to signal flat (non-clustered) mode.
func (cfg *Config) ClusterPaths() map[string][]string {
	if cfg == nil {
		return nil
	}

	hasPrefix := false
	for _, p := range cfg.Paths {
		if c, _ := ParseClusterPath(p); c != "" {
			hasPrefix = true
			break
		}
	}

	if !hasPrefix && len(cfg.Clusters) == 0 {
		return nil
	}

	result := make(map[string][]string)

	for _, p := range cfg.Paths {
		cluster, path := ParseClusterPath(p)
		result[cluster] = append(result[cluster], path)
	}

	for cluster, cc := range cfg.Clusters {
		result[cluster] = append(result[cluster], cc.Paths...)
	}

	return result
}
