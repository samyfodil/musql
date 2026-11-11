package engine

import "sync"

// planPool keeps the plan cache of a closed session for the next session on
// the same database, so a connection opened after another one closed does not
// compile again what the first already did. A cache moves to one session at a
// time and is never shared: a run fills in parts of a Program it reads again
// (its native code, an IN's set), which a second session running it at the
// same moment would race on. planCacheFor checks the cache against the taking
// session's schema and flags before its first use, as it does every statement.
var planPool struct {
	mu sync.Mutex
	m  map[string]*sharedPlanCache
}

// planPoolMax bounds the databases planPool holds a cache for.
const planPoolMax = 64

// poolPlans offers c, a closing session's cache, for the next session on path.
func poolPlans(path string, c *sharedPlanCache) {
	if path == "" || c == nil || len(c.m) == 0 {
		return
	}
	planPool.mu.Lock()
	defer planPool.mu.Unlock()
	if _, ok := planPool.m[path]; !ok && len(planPool.m) >= planPoolMax || planPool.m == nil {
		planPool.m = make(map[string]*sharedPlanCache)
	}
	planPool.m[path] = c
}

// takePooledPlans hands over the cache a closed session left for path, or nil.
func takePooledPlans(path string) *sharedPlanCache {
	if path == "" {
		return nil
	}
	planPool.mu.Lock()
	defer planPool.mu.Unlock()
	c := planPool.m[path]
	delete(planPool.m, path)
	return c
}
