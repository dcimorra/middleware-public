package main

import (
	"net/http"
	"strconv"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/httpcache"
	"github.com/dcimorra/the-middleware/internal/proxy"
)

// theCache is the house's response cache; the API reads its numbers.
var theCache *httpcache.Cache

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "cache", Order: orderCache, Build: buildCache})
}

// buildCache puts the response cache inside the door and inside the
// traffic count: behind access, so nothing kept is ever served past a
// password, and counted, so a hit is traffic like any other.
func buildCache(a *app.App) app.Hook {
	c := httpcache.New(a.Settings.Cache.Bytes())
	theCache = c
	lookup := func(r *http.Request) (httpcache.Site, bool) {
		sn, ok := a.Engine.Lookup(proxy.Host(r))
		if !ok || sn.Manifest == nil || sn.Manifest.Cache == nil || len(sn.Manifest.Cache.Rules) == 0 {
			return httpcache.Site{}, false
		}
		return httpcache.Site{
			Domain:     sn.Domain,
			Rules:      sn.Manifest.Cache.Rules,
			MaxBytes:   int64(sn.Manifest.Cache.MaxMB) << 20,
			Generation: sn.Revision + ":" + strconv.Itoa(sn.Port),
		}, true
	}
	return hook{wrap: func(next http.Handler) http.Handler { return c.Wrap(next, lookup) }}
}
