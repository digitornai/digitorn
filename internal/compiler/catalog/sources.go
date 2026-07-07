package catalog

import (
	"github.com/digitornai/digitorn/pkg/module"
)


type DirSource struct{ Dir string }

func (s DirSource) Manifests() ([]module.Manifest, error) {
	return module.LoadManifestDir(s.Dir)
}

type RegistrySource struct{ Registry *module.Registry }

func (s RegistrySource) Manifests() ([]module.Manifest, error) {
	if s.Registry == nil {
		return nil, nil
	}
	return s.Registry.Manifests(), nil
}


type SliceSource struct{ Items []module.Manifest }

func (s SliceSource) Manifests() ([]module.Manifest, error) { return s.Items, nil }
