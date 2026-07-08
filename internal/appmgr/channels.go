package appmgr

import "gopkg.in/yaml.v3"

// channelCfg routes a built-in to server (prod auto-install) and/or hub (marketplace).
type channelCfg struct {
	Server bool `yaml:"server"`
	Hub    bool `yaml:"hub"`
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
