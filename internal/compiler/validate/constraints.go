package validate

import (
	"fmt"

	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/pkg/module"
)

func CheckConstraints(file string, def *schema.AppDefinition, cat *catalog.Catalog, bag *diagnostic.Bag) {
	if def.Tools == nil {
		return
	}
	for modID, block := range def.Tools.Modules {
		for name, value := range block.Constraints {
			typ, ok := cat.ConstraintType(name)
			if !ok {
				continue
			}
			path := fmt.Sprintf("tools.modules.%s.constraints.%s", modID, name)
			if err := validateConstraintValue(typ, value); err != nil {
				bag.Add(diagnostic.Errorf(diagnostic.CodeWrongType, posUnknown,
					"%s: %s", path, err.Error()))
			}
		}
	}
}

func validateConstraintValue(typ module.ConstraintType, value any) error {
	switch typ {
	case module.ConstraintSize:
		if _, err := module.ParseSize(value); err != nil {
			return err
		}
	case module.ConstraintDuration:
		if _, err := module.ParseDurationValue(value); err != nil {
			return err
		}
	case module.ConstraintInteger:
		switch v := value.(type) {
		case int, int64, float64:
			return nil
		default:
			return fmt.Errorf("expected integer, got %T (%v)", v, v)
		}
	case module.ConstraintBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T (%v)", value, value)
		}
	case module.ConstraintString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected string, got %T (%v)", value, value)
		}
	case module.ConstraintStringList:
		switch v := value.(type) {
		case []any:
			for i, it := range v {
				if _, ok := it.(string); !ok {
					return fmt.Errorf("element %d: expected string, got %T", i, it)
				}
			}
		case []string:
			return nil
		case string:
			return nil
		default:
			return fmt.Errorf("expected list of strings, got %T", value)
		}
	}
	return nil
}
