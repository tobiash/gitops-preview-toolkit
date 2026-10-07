// Package fluxrender implements the persistent Flux rendering plugin.
package fluxrender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander/gitrepo"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander/helm"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	helmcli "helm.sh/helm/v4/pkg/cli"
)

// Config is trusted, engine-specific configuration supplied at OpenRender.
type Config struct {
	// FluxKS defaults to enabled. The optional switch preserves the legacy host's
	// explicit WithFluxKS setting without requiring it in neutral invocations.
	FluxKS       *bool        `json:"fluxKS,omitempty"`
	Helm         bool         `json:"helm"`
	ResolveGit   bool         `json:"resolveGit"`
	HelmSettings HelmSettings `json:"helmSettings"`
}

// HelmSettings contains acquisition settings, independent of Helm's CLI types.
type HelmSettings struct {
	RegistryConfig   string `json:"RegistryConfig"`
	RepositoryConfig string `json:"RepositoryConfig"`
	RepositoryCache  string `json:"RepositoryCache"`
}

// Service retains one explicit run's acquisition caches. Calls are serialized so
// resetting a run or closing cannot remove repositories during a build.
type Service struct {
	mu       sync.Mutex
	log      logr.Logger
	epoch    *acquisitionEpoch
	sessions map[string]*session
	next     uint64
	closed   bool
}

type session struct {
	options         plugin.OpenRequest
	config          Config
	git             *gitrepo.Expander
	runner          *helm.Runner
	epoch           *acquisitionEpoch
	owned           map[ownedIdentity]bool
	helmEvaluations map[string]map[string]helmEvaluation
}

type ownedIdentity struct {
	id         string
	provenance plugin.Provenance
}

var _ plugin.Service = (*Service)(nil)

// New creates a service. Acquisition caches are allocated lazily per run.
func New(log logr.Logger) (*Service, error) {
	return &Service{log: log, sessions: map[string]*session{}}, nil
}

func (s *Service) Describe(ctx context.Context, req *plugin.DescribeRequest) (*plugin.DescribeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || req.ProtocolVersion != plugin.ProtocolVersion {
		return nil, fmt.Errorf("unsupported plugin protocol version")
	}
	return &plugin.DescribeResponse{ProtocolVersion: plugin.ProtocolVersion, Name: "flux", Version: "1"}, nil
}

func (s *Service) OpenRender(ctx context.Context, req *plugin.OpenRequest) (*plugin.OpenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, fmt.Errorf("flux service is closed")
	}
	if req == nil || req.Root == "" {
		return nil, fmt.Errorf("render root is required")
	}
	options := *req
	root, err := filepath.Abs(req.Root)
	if err != nil {
		return nil, err
	}
	options.Root = root
	options.Paths = append([]string{}, req.Paths...)
	options.Config = append(json.RawMessage{}, req.Config...)
	var config Config
	if len(req.Config) > 0 {
		if err := json.Unmarshal(req.Config, &config); err != nil {
			return nil, fmt.Errorf("flux config: %w", err)
		}
	}
	se := &session{options: options, config: config, owned: map[ownedIdentity]bool{},
		helmEvaluations: map[string]map[string]helmEvaluation{}}
	se.epoch, err = s.epochFor(req.RunID)
	if err != nil {
		return nil, err
	}
	if config.ResolveGit || options.LocalOnly {
		if options.LocalOnly {
			se.git = gitrepo.NewLocalExpander(root, s.log)
		} else {
			se.git = se.epoch.git.WithSourceRoot(root)
		}
	}
	settings := helmcli.New()
	if config.HelmSettings.RegistryConfig != "" {
		settings.RegistryConfig = config.HelmSettings.RegistryConfig
	}
	if config.HelmSettings.RepositoryConfig != "" {
		settings.RepositoryConfig = config.HelmSettings.RepositoryConfig
	}
	if config.HelmSettings.RepositoryCache != "" {
		settings.RepositoryCache = config.HelmSettings.RepositoryCache
	}
	se.runner = helm.NewRunner(settings, s.log)
	if options.LocalOnly {
		se.runner.SetLocalOnly(root)
	}
	s.next++
	id := strconv.FormatUint(s.next, 10)
	s.sessions[id] = se
	return &plugin.OpenResponse{Session: id}, nil
}

func (s *Service) Expand(ctx context.Context, req *plugin.ExpandRequest) (*plugin.ExpandResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("expand request is required")
	}
	se, ok := s.sessions[req.Session]
	if !ok || s.closed {
		return nil, fmt.Errorf("unknown render session %q", req.Session)
	}
	// Work transactionally: cancellation must not publish half an evaluation.
	working := *se
	working.helmEvaluations = cloneHelmEvaluations(se.helmEvaluations)
	response, err := s.expand(ctx, &working, req.Resources)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, expansion := range response.Expansions {
		active[expansion.Trigger] = true
	}
	for trigger, evaluations := range working.helmEvaluations {
		if !active[trigger] {
			delete(working.helmEvaluations, trigger)
			continue
		}
		for key, evaluation := range evaluations {
			// Failed or pending acquisitions must be retryable on the next outer
			// sweep. Only successfully rendered desired output is session-pinned.
			if len(evaluation.diagnostics) != 0 {
				delete(evaluations, key)
			}
		}
	}
	// The host owns response slices; keep independent cache storage.
	se.helmEvaluations = cloneHelmEvaluations(working.helmEvaluations)
	// Remember retracted identities too: an outer inventory can still contain
	// yesterday's output while it is applying this exhaustive replacement.
	for _, expansion := range response.Expansions {
		for _, resource := range expansion.Resources {
			se.owned[ownedIdentity{id: resource.ID, provenance: resource.Provenance}] = true
		}
	}
	return response, nil
}

func (s *Service) CloseRender(ctx context.Context, req *plugin.CloseRequest) (*plugin.CloseResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("close request is required")
	}
	se, ok := s.sessions[req.Session]
	delete(s.sessions, req.Session)
	if ok && se.epoch.runID == "" {
		if err := se.epoch.Close(); err != nil {
			return nil, err
		}
	}
	return &plugin.CloseResponse{}, nil
}

// Close releases shared repositories after active calls finish. It is idempotent.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var err error
	for _, se := range s.sessions {
		if se.epoch.runID == "" {
			err = errors.Join(err, se.epoch.Close())
		}
	}
	clear(s.sessions)
	if s.epoch != nil {
		err = errors.Join(err, s.epoch.Close())
	}
	return err
}
