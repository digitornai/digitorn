package catalog

import (
	"github.com/digitornai/digitorn/pkg/module"
)

// DirSource loads manifests from a directory of YAML files.
type DirSource struct{ Dir string }

func (s DirSource) Manifests() ([]module.Manifest, error) {
	return module.LoadManifestDir(s.Dir)
}

// RegistrySource exposes a *module.Registry as a ManifestSource.
type RegistrySource struct{ Registry *module.Registry }

func (s RegistrySource) Manifests() ([]module.Manifest, error) {
	if s.Registry == nil {
		return nil, nil
	}
	return s.Registry.Manifests(), nil
}

// SliceSource is a trivial source for tests or programmatic use.
type SliceSource struct{ Items []module.Manifest }

func (s SliceSource) Manifests() ([]module.Manifest, error) { return s.Items, nil }
