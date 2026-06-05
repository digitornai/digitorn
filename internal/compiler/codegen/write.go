package codegen

import (
	"fmt"
	"os"
)

func WriteFile(path string, a *Artifact) error {
	data, err := EncodeBytes(a)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("codegen: write %s: %w", path, err)
	}
	return nil
}

func ReadFile(path string) (*Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("codegen: open %s: %w", path, err)
	}
	defer f.Close()
	return Decode(f)
}
