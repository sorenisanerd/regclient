// Package reg implements the OCI registry scheme used by most images (host:port/repo:tag)
package reg

import (
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/internal/cache"
	"github.com/regclient/regclient/internal/reghttp"
	"github.com/regclient/regclient/internal/throttle"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
	"github.com/regclient/regclient/types/referrer"
)

const (
	// blobChunkMinHeader is returned by registries requesting a minimum chunk size
	blobChunkMinHeader = "OCI-Chunk-Min-Length"
	// defaultBlobChunk 1M chunks, this is allocated in a memory buffer
	defaultBlobChunk = 1024 * 1024
	// defaultBlobChunkLimit 1G chunks, prevents a memory exhaustion attack
	defaultBlobChunkLimit = 1024 * 1024 * 1024
	// defaultBlobMax is disabled to support registries without chunked upload support
	defaultBlobMax = -1
	// defaultManifestMaxPull limits the largest manifest that will be pulled
	defaultManifestMaxPull = 1024 * 1024 * 8
	// defaultManifestMaxPush limits the largest manifest that will be pushed
	defaultManifestMaxPush = 1024 * 1024 * 4
)

// Reg is used for interacting with remote registry servers
type Reg struct {
	reghttp         *reghttp.Client
	reghttpOpts     []reghttp.Opts
	log             *logrus.Logger
	hosts           map[string]*config.Host
	features        map[featureKey]*featureVal
	blobChunkSize   int64
	blobChunkLimit  int64
	blobMaxPut      int64
	manifestMaxPull int64
	manifestMaxPush int64
	cacheMan        *cache.Cache[ref.Ref, manifest.Manifest]
	cacheRL         *cache.Cache[ref.Ref, referrer.ReferrerList]
	muHost          sync.Mutex
	muRefTag        sync.Mutex
}

type featureKey struct {
	kind string
	reg  string
	repo string
}
type featureVal struct {
	enabled bool
	expire  time.Time
}

var featureExpire = time.Minute * time.Duration(5)

// Opts provides options to access registries
type Opts func(*Reg)

// New returns a Reg pointer with any provided options
func New(opts ...Opts) *Reg {
	r := Reg{
		reghttpOpts:     []reghttp.Opts{},
		blobChunkSize:   defaultBlobChunk,
		blobChunkLimit:  defaultBlobChunkLimit,
		blobMaxPut:      defaultBlobMax,
		manifestMaxPull: defaultManifestMaxPull,
		manifestMaxPush: defaultManifestMaxPush,
		hosts:           map[string]*config.Host{},
		features:        map[featureKey]*featureVal{},
	}
	r.reghttpOpts = append(r.reghttpOpts, reghttp.WithConfigHost(r.hostGet))
	for _, opt := range opts {
		opt(&r)
	}
	r.reghttp = reghttp.NewClient(r.reghttpOpts...)
	return &r
}

// Throttle is used to limit concurrency
func (reg *Reg) Throttle(r ref.Ref, put bool) []*throttle.Throttle {
	tList := []*throttle.Throttle{}
	host := reg.hostGet(r.Registry)
	t := host.Throttle()
	if t != nil {
		tList = append(tList, t)
	}
	if !put {
		for _, mirror := range host.Mirrors {
			t := reg.hostGet(mirror).Throttle()
			if t != nil {
				tList = append(tList, t)
			}
		}
	}
	return tList
}

func (reg *Reg) hostGet(hostname string) *config.Host {
	reg.muHost.Lock()
	defer reg.muHost.Unlock()
	if _, ok := reg.hosts[hostname]; !ok {
		reg.hosts[hostname] = config.HostNewName(hostname)
	}
	return reg.hosts[hostname]
}

// featureGet returns enabled and ok
func (reg *Reg) featureGet(kind, registry, repo string) (bool, bool) {
	reg.muHost.Lock()
	defer reg.muHost.Unlock()
	if v, ok := reg.features[featureKey{kind: kind, reg: registry, repo: repo}]; ok {
		if time.Now().Before(v.expire) {
			return v.enabled, true
		}
	}
	return false, false
}

func (reg *Reg) featureSet(kind, registry, repo string, enabled bool) {
	reg.muHost.Lock()
	reg.features[featureKey{kind: kind, reg: registry, repo: repo}] = &featureVal{enabled: enabled, expire: time.Now().Add(featureExpire)}
	reg.muHost.Unlock()
}

// WithBlobSize overrides default blob sizes
func WithBlobSize(size, max int64) Opts {
	return func(r *Reg) {
		if size > 0 {
			r.blobChunkSize = size
		}
		if max != 0 {
			r.blobMaxPut = max
		}
	}
}

// WithBlobLimit overrides default blob limit
func WithBlobLimit(limit int64) Opts {
	return func(r *Reg) {
		if limit > 0 {
			r.blobChunkLimit = limit
		}
		if r.blobMaxPut > 0 && r.blobMaxPut < limit {
			r.blobMaxPut = limit
		}
	}
}

// WithCache defines a cache used for various requests
func WithCache(timeout time.Duration, count int) Opts {
	return func(r *Reg) {
		cm := cache.New[ref.Ref, manifest.Manifest](cache.WithAge(timeout), cache.WithCount(count))
		r.cacheMan = &cm
		crl := cache.New[ref.Ref, referrer.ReferrerList](cache.WithAge(timeout), cache.WithCount(count))
		r.cacheRL = &crl
	}
}

// WithCerts adds certificates
func WithCerts(certs [][]byte) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithCerts(certs))
	}
}

// WithCertDirs adds certificate directories for host specific certs
func WithCertDirs(dirs []string) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithCertDirs(dirs))
	}
}

// WithCertFiles adds certificates by filename
func WithCertFiles(files []string) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithCertFiles(files))
	}
}

// WithConfigHosts adds host configs for credentials
func WithConfigHosts(configHosts []*config.Host) Opts {
	return func(r *Reg) {
		for _, host := range configHosts {
			if host.Name == "" {
				continue
			}
			r.hosts[host.Name] = host
		}
	}
}

// WithDelay initial time to wait between retries (increased with exponential backoff)
func WithDelay(delayInit time.Duration, delayMax time.Duration) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithDelay(delayInit, delayMax))
	}
}

// WithHTTPClient uses a specific http client with retryable requests
func WithHTTPClient(hc *http.Client) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithHTTPClient(hc))
	}
}

// WithLog injects a logrus Logger configuration
func WithLog(log *logrus.Logger) Opts {
	return func(r *Reg) {
		r.log = log
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithLog(log))
	}
}

// WithManifestMax sets the push and pull limits for manifests
func WithManifestMax(push, pull int64) Opts {
	return func(r *Reg) {
		r.manifestMaxPush = push
		r.manifestMaxPull = pull
	}
}

// WithRetryLimit restricts the number of retries (defaults to 5)
func WithRetryLimit(l int) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithRetryLimit(l))
	}
}

// WithTransport uses a specific http transport with retryable requests
func WithTransport(t *http.Transport) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithTransport(t))
	}
}

// WithUserAgent sets a user agent header
func WithUserAgent(ua string) Opts {
	return func(r *Reg) {
		r.reghttpOpts = append(r.reghttpOpts, reghttp.WithUserAgent(ua))
	}
}
