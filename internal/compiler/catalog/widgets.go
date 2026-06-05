package catalog

// WidgetFilters is the closed set of template filters allowed inside widget
// expressions such as `{{ value | upper | truncate:80 }}`.
var WidgetFilters = []string{
	"upper", "lower", "title", "capitalize", "truncate",
	"default", "length", "date", "relative_time", "time", "money", "number",
	"percent", "json", "yaml", "filter", "map", "pluck",
	"join", "first", "last", "sort", "reverse", "slice",
	"replace", "markdown", "plus_days", "minus_days",
	"filter_search", "source_icon", "tree_icon", "kind_color",
	"status_color", "sev_color",
}

func (c *Catalog) HasWidgetFilter(name string) bool {
	_, ok := c.widgetFilters[name]
	return ok
}

func (c *Catalog) WidgetFilters() []string { return WidgetFilters }
