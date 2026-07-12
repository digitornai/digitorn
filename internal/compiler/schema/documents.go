package schema

import "github.com/digitornai/digitorn/internal/docstore"

// DocumentDecl declares a fragmented JSON document (docstore): the daemon
// seeds/explodes it at session bind and keeps fragments ↔ composed in sync.
// Flat named fields (no embedding) — the compiler's unknown-field walker
// reflects over this struct.
type DocumentDecl struct {
	Match string `yaml:"match" json:"match"`
	// Seed is the workdir-relative composed filename bound at session start.
	Seed         string          `yaml:"seed,omitempty" json:"seed,omitempty"`
	Root         string          `yaml:"root,omitempty" json:"root,omitempty"`
	RootDefaults map[string]any  `yaml:"root_defaults,omitempty" json:"root_defaults,omitempty"`
	Overview     string          `yaml:"overview,omitempty" json:"overview,omitempty"`
	Collections  []DocCollection `yaml:"collections,omitempty" json:"collections,omitempty"`
	Validate     *DocValidate    `yaml:"validate,omitempty" json:"validate,omitempty"`
	Layout       *DocLayout      `yaml:"layout,omitempty" json:"layout,omitempty"`
	Defaults     *DocDefaults    `yaml:"defaults,omitempty" json:"defaults,omitempty"`
}

// DocDefaults completes declarative fragments into full renderable elements.
// Generic: every field name is app-specific and lives here, in the app YAML.
type DocDefaults struct {
	TypeField string                    `yaml:"type_field,omitempty" json:"type_field,omitempty"`
	Common    map[string]any            `yaml:"common,omitempty" json:"common,omitempty"`
	ByType    map[string]map[string]any `yaml:"by_type,omitempty" json:"by_type,omitempty"`
	Generated map[string][]string       `yaml:"generated,omitempty" json:"generated,omitempty"`
}

type DocLayout struct {
	Route   string    `yaml:"route,omitempty" json:"route,omitempty"`
	Gap     float64   `yaml:"gap,omitempty" json:"gap,omitempty"`
	Grid    *DocGrid  `yaml:"grid,omitempty" json:"grid,omitempty"`
	Edge    *DocEdge  `yaml:"edge,omitempty" json:"edge,omitempty"`
	Label   *DocLabel `yaml:"label,omitempty" json:"label,omitempty"`
	Derived []string  `yaml:"derived,omitempty" json:"derived,omitempty"`
}

type DocLabel struct {
	Field    string  `yaml:"field" json:"field"`
	TextKey  string  `yaml:"text_key,omitempty" json:"text_key,omitempty"`
	In       string  `yaml:"in,omitempty" json:"in,omitempty"`
	Type     string  `yaml:"type,omitempty" json:"type,omitempty"`
	IDSuffix string  `yaml:"id_suffix,omitempty" json:"id_suffix,omitempty"`
	Ref      string  `yaml:"ref,omitempty" json:"ref,omitempty"`
	BindType string  `yaml:"bind_type,omitempty" json:"bind_type,omitempty"`
	Align    string  `yaml:"align,omitempty" json:"align,omitempty"`
	VAlign   string  `yaml:"valign,omitempty" json:"valign,omitempty"`
	FontSize float64 `yaml:"font_size,omitempty" json:"font_size,omitempty"`
	Pad      float64 `yaml:"pad,omitempty" json:"pad,omitempty"`
}

type DocGrid struct {
	Field    string  `yaml:"field" json:"field"`
	CellW    float64 `yaml:"cell_w,omitempty" json:"cell_w,omitempty"`
	CellH    float64 `yaml:"cell_h,omitempty" json:"cell_h,omitempty"`
	GutterX  float64 `yaml:"gutter_x,omitempty" json:"gutter_x,omitempty"`
	GutterY  float64 `yaml:"gutter_y,omitempty" json:"gutter_y,omitempty"`
	OriginX  float64 `yaml:"origin_x,omitempty" json:"origin_x,omitempty"`
	OriginY  float64 `yaml:"origin_y,omitempty" json:"origin_y,omitempty"`
	DefaultW float64 `yaml:"default_w,omitempty" json:"default_w,omitempty"`
	DefaultH float64 `yaml:"default_h,omitempty" json:"default_h,omitempty"`
}

type DocEdge struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
	In   string `yaml:"in,omitempty" json:"in,omitempty"`
}

type DocCollection struct {
	Name  string `yaml:"name" json:"name"`
	Path  string `yaml:"path" json:"path"`
	ID    string `yaml:"id,omitempty" json:"id,omitempty"`
	Grain string `yaml:"grain,omitempty" json:"grain,omitempty"`
	Order string `yaml:"order,omitempty" json:"order,omitempty"`
}

type DocValidate struct {
	UniqueID bool     `yaml:"unique_id,omitempty" json:"unique_id,omitempty"`
	Refs     []DocRef `yaml:"refs,omitempty" json:"refs,omitempty"`
}

type DocRef struct {
	Field string `yaml:"field" json:"field"`
	In    string `yaml:"in" json:"in"`
}

// Manifest converts the declaration to the docstore engine's manifest.
func (d DocumentDecl) Manifest() docstore.Manifest {
	m := docstore.Manifest{Match: d.Match, Root: d.Root, Overview: d.Overview, RootDefaults: d.RootDefaults}
	for _, c := range d.Collections {
		m.Collections = append(m.Collections, docstore.Collection{
			Name: c.Name, Path: c.Path, ID: c.ID, Grain: c.Grain, Order: c.Order,
		})
	}
	if d.Validate != nil {
		m.Validate.UniqueID = d.Validate.UniqueID
		for _, r := range d.Validate.Refs {
			m.Validate.Refs = append(m.Validate.Refs, docstore.Ref{Field: r.Field, In: r.In})
		}
	}
	if d.Layout != nil {
		l := &docstore.Layout{Route: d.Layout.Route, Gap: d.Layout.Gap, Derived: d.Layout.Derived}
		if g := d.Layout.Grid; g != nil {
			l.Grid = &docstore.GridSpec{
				Field: g.Field, CellW: g.CellW, CellH: g.CellH,
				GutterX: g.GutterX, GutterY: g.GutterY, OriginX: g.OriginX, OriginY: g.OriginY,
				DefaultW: g.DefaultW, DefaultH: g.DefaultH,
			}
		}
		if e := d.Layout.Edge; e != nil {
			l.Edge = &docstore.EdgeSpec{From: e.From, To: e.To, In: e.In}
		}
		if lb := d.Layout.Label; lb != nil {
			l.Label = &docstore.LabelSpec{
				Field: lb.Field, TextKey: lb.TextKey, In: lb.In, Type: lb.Type,
				IDSuffix: lb.IDSuffix, Ref: lb.Ref, BindType: lb.BindType,
				Align: lb.Align, VAlign: lb.VAlign, FontSize: lb.FontSize, Pad: lb.Pad,
			}
		}
		m.Layout = l
	}
	if d.Defaults != nil {
		m.Defaults = &docstore.Defaults{
			TypeField: d.Defaults.TypeField,
			Common:    d.Defaults.Common,
			ByType:    d.Defaults.ByType,
			Generated: d.Defaults.Generated,
		}
	}
	return m
}
