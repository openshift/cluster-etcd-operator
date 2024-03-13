package health

import (
	"runtime"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type AlivenessChecker interface {
	Alive() bool
}

type MultiAlivenessChecker struct {
	m sync.Mutex
	// name -> checker
	checkerMap map[string]AlivenessChecker

	lastPrintTime time.Time
}

func (r *MultiAlivenessChecker) Add(name string, c AlivenessChecker) {
	r.m.Lock()
	defer r.m.Unlock()

	r.checkerMap[name] = c
}

func (r *MultiAlivenessChecker) Alive() bool {
	r.m.Lock()
	defer r.m.Unlock()

	for s, checker := range r.checkerMap {
		if !checker.Alive() {
			klog.Warningf("Controller [%s] didn't sync for a long time, declaring unhealthy and dumping stack", s)

			// we throttle this to once every 15m because dumping 12mb of logs for every probe failure is very expensive
			if r.lastPrintTime.Add(time.Minute * 15).After(time.Now()) {
				// 12 mb should be enough for a full goroutine dump
				buf := make([]byte, 1024*1024*12)
				n := runtime.Stack(buf, true)
				klog.Warningf("%s", buf[:n])
				r.lastPrintTime = time.Now()
			}
			return false
		}
	}

	return true
}

func NewMultiAlivenessChecker() *MultiAlivenessChecker {
	return &MultiAlivenessChecker{
		m:             sync.Mutex{},
		checkerMap:    make(map[string]AlivenessChecker),
		lastPrintTime: time.UnixMilli(0),
	}
}
