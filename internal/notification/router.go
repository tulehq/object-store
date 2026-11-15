package notification

import (
	"crypto/sha256"
	"maps"
	"sync"

	"tule.dev/object-store/internal/contracts"
)

// cacheSize is how many decoded configurations a Router keeps.
const cacheSize = 256

// Router is the contracts.EventRouter of the buckets of one account. It
// keeps the configurations it decoded, by the sha256 of their stored form,
// so an event does not decode its bucket's configuration again. It is
// safe for concurrent use.
type Router struct {
	available map[contracts.DestinationType]bool
	cache     map[[sha256.Size]byte]*decoded
	owner     string
	order     [][sha256.Size]byte // cache keys, oldest first
	mu        sync.Mutex
}

// decoded is a stored configuration Decode read.
type decoded struct {
	config     *Configuration
	generation string
}

var _ contracts.EventRouter = (*Router)(nil)

// NewRouter returns the Router of the buckets of account, which routes
// events to the destination types available names true: those whose
// service has an endpoint. Topics are never routed to.
func NewRouter(account string, available map[contracts.DestinationType]bool) *Router {
	return &Router{
		available: maps.Clone(available),
		cache:     make(map[[sha256.Size]byte]*decoded, cacheSize),
		owner:     OwnerPrincipalID(account),
	}
}

// Route implements contracts.EventRouter.
func (r *Router) Route(
	doc []byte, generation string, ev *contracts.BucketEvent,
) (contracts.Destination, bool, error) {
	d, err := r.decode(doc)
	if err != nil {
		return contracts.Destination{}, false, err
	}
	if d.generation != generation {
		return contracts.Destination{}, false, nil
	}
	dest, ok := d.config.Match(ev.Name, ev.Key)
	if !ok || dest.Type == contracts.DestinationTopic || !r.available[dest.Type] {
		return contracts.Destination{}, false, nil
	}
	return dest, true, nil
}

// Render implements contracts.EventRouter.
func (r *Router) Render(d contracts.Destination, ev *contracts.BucketEvent) ([]byte, error) {
	return render(r.owner, d.ConfigurationID, ev)
}

// decode returns the configuration doc stores, from the cache when it has
// it; the oldest entry makes room for a new one.
func (r *Router) decode(doc []byte) (*decoded, error) {
	key := sha256.Sum256(doc)
	r.mu.Lock()
	d, ok := r.cache[key]
	r.mu.Unlock()
	if ok {
		return d, nil
	}
	s, c, err := Decode(doc)
	if err != nil {
		return nil, err
	}
	d = &decoded{config: c, generation: s.Generation}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cache[key]; !ok {
		if len(r.order) == cacheSize {
			delete(r.cache, r.order[0])
			r.order = r.order[1:]
		}
		r.cache[key] = d
		r.order = append(r.order, key)
	}
	return d, nil
}
