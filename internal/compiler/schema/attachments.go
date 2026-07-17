package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type Attachments []AttachmentType

func (a *Attachments) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Value == "*" || node.Value == "all" {
			*a = []AttachmentType{AttachmentImage, AttachmentDocument, AttachmentAudio, AttachmentVideo}
			return nil
		}
		if node.Value == "" || node.Value == "~" || node.Value == "null" {
			return nil
		}
		*a = []AttachmentType{AttachmentType(node.Value)}
		return nil
	case yaml.SequenceNode:
		for _, it := range node.Content {
			if it.Kind == yaml.ScalarNode {
				*a = append(*a, AttachmentType(it.Value))
			}
		}
		return nil
	default:
		return fmt.Errorf("attachments: expected scalar, sequence or null, got kind %d", node.Kind)
	}
}
