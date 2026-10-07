package fluxrender

import (
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander/gitrepo"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander/helm"
)

// An acquisition epoch pins mutable selectors for one explicit preview run.
// A service retains at most one named epoch, including between its before/head
// sessions. Standalone sessions without a run identity own private epochs.
type acquisitionEpoch struct {
	runID  string
	git    *gitrepo.Expander
	charts *helm.AcquisitionCache
}

func newAcquisitionEpoch(runID string, log logr.Logger) (*acquisitionEpoch, error) {
	git, err := gitrepo.NewExpander(log)
	if err != nil {
		return nil, err
	}
	charts, err := helm.NewAcquisitionCache()
	if err != nil {
		return nil, errors.Join(err, git.Close())
	}
	return &acquisitionEpoch{runID: runID, git: git, charts: charts}, nil
}

func (e *acquisitionEpoch) Close() error {
	return errors.Join(e.git.Close(), e.charts.Close())
}

// epochFor is called under the service operation lock, so retiring an epoch
// cannot race a renderer reading its archives or cloned repositories.
func (s *Service) epochFor(runID string) (*acquisitionEpoch, error) {
	if runID == "" {
		return newAcquisitionEpoch("", s.log)
	}
	if s.epoch != nil {
		if s.epoch.runID == runID {
			return s.epoch, nil
		}
		for _, se := range s.sessions {
			if se.epoch == s.epoch {
				return nil, fmt.Errorf("cannot open acquisition run %q while run %q has active sessions", runID, s.epoch.runID)
			}
		}
	}
	next, err := newAcquisitionEpoch(runID, s.log)
	if err != nil {
		return nil, err
	}
	previous := s.epoch
	s.epoch = nil
	if previous != nil {
		if err := previous.Close(); err != nil {
			return nil, errors.Join(err, next.Close())
		}
	}
	s.epoch = next
	return next, nil
}
