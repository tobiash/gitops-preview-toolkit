/*
Copyright 2021 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helm

import (
	"bytes"
	"encoding/json"
	"sync"

	v2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/kustomize"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

type postRendererKustomize struct {
	spec      *v2.Kustomize
	localOnly bool
}

func newPostRendererKustomize(spec *v2.Kustomize) *postRendererKustomize {
	return &postRendererKustomize{spec: spec}
}

func writeOutput(fs filesys.FileSystem, path string, content []byte) error {
	f, err := fs.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	return f.Close()
}

func adaptImages(images []kustomize.Image) []kustypes.Image {
	output := make([]kustypes.Image, len(images))
	for i, image := range images {
		output[i] = kustypes.Image{
			Name:    image.Name,
			NewName: image.NewName,
			NewTag:  image.NewTag,
			Digest:  image.Digest,
		}
	}
	return output
}

func adaptSelector(selector *kustomize.Selector) *kustypes.Selector {
	if selector == nil {
		return nil
	}
	return &kustypes.Selector{
		AnnotationSelector: selector.AnnotationSelector,
		LabelSelector:      selector.LabelSelector,
	}
}

func (k *postRendererKustomize) Run(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error) {
	fs := filesys.MakeFsInMemory()
	cfg := kustypes.Kustomization{}
	cfg.APIVersion = kustypes.KustomizationVersion
	cfg.Kind = kustypes.KustomizationKind
	cfg.Images = adaptImages(k.spec.Images)

	// Add rendered Helm output as input resource to the Kustomization.
	const input = "helm-output.yaml"
	cfg.Resources = append(cfg.Resources, input)
	if err := writeOutput(fs, input, renderedManifests.Bytes()); err != nil {
		return nil, err
	}

	// Add patches (v2 GA only has Patches, not StrategicMerge/JSON6902).
	for _, m := range k.spec.Patches {
		cfg.Patches = append(cfg.Patches, kustypes.Patch{
			Patch:  m.Patch,
			Target: adaptSelector(m.Target),
		})
	}

	// Write kustomization config to file.
	kustomization, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := writeOutput(fs, "kustomization.yaml", kustomization); err != nil {
		return nil, err
	}
	if k.localOnly {
		if err := render.ValidateLocalKustomization(fs, ".", "."); err != nil {
			return nil, err
		}
	}
	resMap, err := buildKustomization(fs, ".")
	if err != nil {
		return nil, err
	}
	yaml, err := resMap.AsYaml()
	if err != nil {
		return nil, err
	}
	return bytes.NewBuffer(yaml), nil
}

// kustomizeRenderMutex works around a concurrent map read/write panic in kustomize.
// https://github.com/kubernetes-sigs/kustomize/issues/3659
var kustomizeRenderMutex sync.Mutex

func buildKustomization(fs filesys.FileSystem, dirPath string) (resmap.ResMap, error) {
	kustomizeRenderMutex.Lock()
	defer kustomizeRenderMutex.Unlock()

	buildOptions := &krusty.Options{
		Reorder:           krusty.ReorderOptionLegacy,
		LoadRestrictions:  kustypes.LoadRestrictionsNone,
		AddManagedbyLabel: false,
		PluginConfig:      kustypes.DisabledPluginConfig(),
	}

	k := krusty.MakeKustomizer(buildOptions)
	return k.Run(fs, dirPath)
}
