package bundle

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Bundle struct {
	Root   string
	Entry  string
	Locale string
}

func Load(path string) (*Bundle, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}

	root, entry := abs, abs
	if info.IsDir() {
		entry = filepath.Join(abs, "app.yaml")
		if _, err := os.Stat(entry); err != nil {
			return nil, fmt.Errorf("bundle %s: app.yaml not found", abs)
		}
	} else {
		root = filepath.Dir(abs)
	}

	return &Bundle{
		Root:   root,
		Entry:  entry,
		Locale: os.Getenv("DIGITORN_LOCALE"),
	}, nil
}

func (b *Bundle) ReadPrompt(name string) (string, error) {
	return b.readLocalized("prompts", name, ".md")
}

func (b *Bundle) ReadSkill(name string) (string, error) {
	return b.readLocalized("skills", name, ".md")
}

func (b *Bundle) ReadBehavior(name string) (string, error) {
	candidates := []string{
		filepath.Join(b.Root, "behavior", name+".yaml"),
		filepath.Join(b.Root, "behavior", name+".yml"),
	}
	for _, c := range candidates {
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		var v any
		if err := yamlUnmarshal(data, &v); err != nil {
			return "", fmt.Errorf("behavior/%s: %w", name, err)
		}
		j, err := jsonMarshal(v)
		if err != nil {
			return "", fmt.Errorf("behavior/%s: %w", name, err)
		}
		return string(j), nil
	}
	return "", fmt.Errorf("behavior file not found: behavior/%s.yaml", name)
}

func (b *Bundle) AssetURL(appID, name string) (string, error) {
	if _, err := os.Stat(filepath.Join(b.Root, "assets", name)); err != nil {
		return "", fmt.Errorf("asset not found: assets/%s", name)
	}
	return fmt.Sprintf("/api/apps/%s/assets/%s", appID, name), nil
}

func (b *Bundle) AssetBase64(name string) (string, error) {
	path := filepath.Join(b.Root, "assets", name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("asset not found: assets/%s", name)
	}
	mime := http.DetectContentType(data)
	return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data)), nil
}

func (b *Bundle) ReadInclude(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("include: path %q escapes bundle root", rel)
	}
	data, err := os.ReadFile(filepath.Join(b.Root, clean))
	if err != nil {
		return "", fmt.Errorf("include: %w", err)
	}
	return string(data), nil
}

func (b *Bundle) readLocalized(dir, name, ext string) (string, error) {
	if b.Locale != "" {
		variant := filepath.Join(b.Root, dir, name+"."+b.Locale+ext)
		if data, err := os.ReadFile(variant); err == nil {
			return string(data), nil
		}
	}
	path := filepath.Join(b.Root, dir, name+ext)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s/%s%s not found", dir, name, ext)
	}
	return string(data), nil
}
