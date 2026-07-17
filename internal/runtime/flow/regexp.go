package flow

import (
	"regexp"
	"sync"
)

var reCache sync.Map

type compiledRe struct {
	re  *regexp.Regexp
	err error
}

func cachedRegexp(pat string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(pat); ok {
		c := v.(compiledRe)
		return c.re, c.err
	}
	re, err := regexp.Compile(pat)
	reCache.Store(pat, compiledRe{re: re, err: err})
	return re, err
}
