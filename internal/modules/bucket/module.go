// Package bucket serves the S3 bucket operations: listing buckets and their
// contents, creating and deleting buckets, reading bucket configuration, and
// batch object deletes.
package bucket

import (
	"context"
	"net/http"

	"github.com/gasmod/gas"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/contracts"
)

const moduleName = "tule/modules/bucket"

// Module registers the routes for these operations and serves them.
type Module struct {
	router  *gas.Router
	logger  gas.Logger
	storage contracts.StorageProvider
	// verifier reads the signed request documents an operation acts on, so
	// their bytes are verified against the signature before they are
	// decoded.
	verifier *sigv4.Verifier
}

// NewModule returns a Module serving from storage, reading request
// documents through verifier. It registers its routes when the app
// initializes it.
func NewModule(
	router *gas.Router,
	logger gas.Logger,
	storage contracts.StorageProvider,
	verifier *sigv4.Verifier,
) *Module {
	return &Module{
		router:   router,
		logger:   logger.With().Str("module", moduleName).Logger(),
		storage:  storage,
		verifier: verifier,
	}
}

// Name identifies the module to gas.
func (m *Module) Name() string {
	return moduleName
}

// Init registers the module's routes.
func (m *Module) Init(context.Context) error {
	m.registerRoutes()
	return nil
}

// Close releases nothing; the module holds no resources of its own.
func (m *Module) Close(context.Context) error {
	return nil
}

func (m *Module) registerRoutes() {
	m.router.Handle(m, http.MethodGet, "/", m.listBuckets) // List buckets

	// Path-style bucket operations, with or without a trailing slash. GET must
	// be registered before HEAD: the router maps HEAD onto every GET route.
	for _, p := range []string{"/{bucket}", "/{bucket}/"} {
		m.router.Handle(
			m,
			http.MethodGet,
			p,
			m.getBucket,
		) // List objects or uploads, or read config
		m.router.Handle(m, http.MethodHead, p, m.headBucket)     // Check a bucket exists
		m.router.Handle(m, http.MethodPut, p, m.putBucket)       // Create bucket
		m.router.Handle(m, http.MethodDelete, p, m.deleteBucket) // Delete (empty) bucket
		m.router.Handle(m, http.MethodPost, p, m.postBucket)     // Delete objects (?delete)
	}
}
