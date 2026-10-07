package helm

import (
	"bytes"
	"encoding/json"
	"fmt"

	v2 "github.com/fluxcd/helm-controller/api/v2"
)

type helmReleasePostRender struct {
	Kustomize *v2.Kustomize `json:"kustomize,omitempty"`
}

type helmReleasePostRenderSpec struct {
	PostRenderers  []helmReleasePostRender `json:"postRenderers,omitempty"`
	CommonMetadata *v2.CommonMetadata      `json:"commonMetadata,omitempty"`
}

type combinedPostRenderer struct {
	renderers []PostRenderer
}

func newCombinedPostRenderer() combinedPostRenderer {
	return combinedPostRenderer{renderers: make([]PostRenderer, 0)}
}

func localPostRenderer(renderer PostRenderer) (PostRenderer, error) {
	switch r := renderer.(type) {
	case nil, *postRendererOriginLabels, *postRendererCommonMetadata:
		return renderer, nil
	case *combinedPostRenderer:
		combined := newCombinedPostRenderer()
		for _, child := range r.renderers {
			local, err := localPostRenderer(child)
			if err != nil {
				return nil, err
			}
			combined.addRenderer(local)
		}
		return &combined, nil
	case *postRendererKustomize:
		return &postRendererKustomize{spec: r.spec, localOnly: true}, nil
	default:
		return nil, fmt.Errorf("local-only: external Helm postrenderers are disabled")
	}
}

func (c *combinedPostRenderer) addRenderer(renderer PostRenderer) {
	c.renderers = append(c.renderers, renderer)
}

func (c *combinedPostRenderer) Run(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error) {
	var result = renderedManifests
	for _, renderer := range c.renderers {
		result, err = renderer.Run(result)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// buildPostRenderers creates the post-renderer chain for a HelmRelease.
// Order: Kustomize patches → CommonMetadata → Origin labels
func buildPostRenderers(hr *v2.HelmRelease) PostRenderer {
	postRenderers := make([]helmReleasePostRender, len(hr.Spec.PostRenderers))
	for i, renderer := range hr.Spec.PostRenderers {
		postRenderers[i] = helmReleasePostRender{Kustomize: renderer.Kustomize}
	}
	return buildPostRenderersFromFields(hr.Name, hr.Namespace, postRenderers, hr.Spec.CommonMetadata)
}

func buildPostRenderersFromSpec(name, namespace string, spec map[string]any) (PostRenderer, error) {
	parsed := helmReleasePostRenderSpec{}
	if spec != nil {
		data, err := json.Marshal(spec)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, err
		}
	}
	return buildPostRenderersFromFields(name, namespace, parsed.PostRenderers, parsed.CommonMetadata), nil
}

func buildPostRenderersFromFields(name, namespace string, postRenderers []helmReleasePostRender, commonMetadata *v2.CommonMetadata) PostRenderer {
	var combined = newCombinedPostRenderer()
	for _, r := range postRenderers {
		if r.Kustomize != nil {
			combined.addRenderer(newPostRendererKustomize(r.Kustomize))
		}
	}
	if commonMetadata != nil {
		combined.addRenderer(newPostRendererCommonMetadata(commonMetadata))
	}
	combined.addRenderer(newPostRendererOriginLabels(name, namespace))
	if len(combined.renderers) == 0 {
		return nil
	}
	return &combined
}
