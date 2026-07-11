package appmgr

import "gopkg.in/yaml.v3"

// channelCfg routes a built-in to server (prod auto-install) and/or hub (marketplace).
// Provision selects HOW a server-channel app is installed on a prod deployment:
//   - "" | "builtin" : from the daemon's embedded source (light foundational apps)
//   - "hub"          : fetched from the hub after boot (heavy apps with web bundles
//     kept out of the binary). Provisioned asynchronously so boot never blocks.
type channelCfg struct {
	Server    bool   `yaml:"server"`
	Hub       bool   `yaml:"hub"`
	Provision string `yaml:"provision"`
}

// hubProvisionedApps returns the server-channel app ids whose install source is
// the hub (provision: hub) — the ones an async provisioner fetches post-boot.
func hubProvisionedApps() []string {
	var out []string
	for name, c := range loadChannels() {
		if c.Server && c.Provision == "hub" {
			out = append(out, name)
		}
	}
	return out
}

// loadChannels parses embedded builtins/channels.yaml; missing/invalid → empty map.
func loadChannels() map[string]channelCfg {
	data, err := builtinFS.ReadFile("builtins/channels.yaml")
	if err != nil {
		return map[string]channelCfg{}
	}
	var doc struct {
		Apps map[string]channelCfg `yaml:"apps"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Apps == nil {
		return map[string]channelCfg{}
	}
	return doc.Apps
}

// builtinVersion reads app.version from an embedded built-in's app.yaml.
func builtinVersion(name string) string {
	data, err := builtinFS.ReadFile("builtins/" + name + "/app.yaml")
	if err != nil {
		return ""
	}
	var doc struct {
		App struct {
			Version string `yaml:"version"`
		} `yaml:"app"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ""
	}
	return doc.App.Version
}
