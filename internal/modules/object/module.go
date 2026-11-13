// Package object serves the S3 object operations: reads, writes, copies,
// deletes and multipart uploads.
package object

import (
	"context"
	"net/http"

	"github.com/gasmod/gas"
	"tule.dev/sigv4"

	"tule.dev/object-store/internal/authz"
	"tule.dev/object-store/internal/contracts"
)

const moduleName = "tule/modules/object"

// Module registers the routes for these operations and serves them.
type Module struct {
	router  *gas.Router
	logger  gas.Logger
	storage contracts.StorageProvider
	// verifier reads the signed request documents an operation acts on, so
	// their bytes are verified against the signature before they are
	// decoded.
	verifier *sigv4.Verifier
	// authz evaluates each operation against the signer's policies.
	authz *authz.Authorizer
}

// NewModule returns a Module serving from storage, reading request
// documents through verifier and authorizing operations with az. It
// registers its routes when the app
// initializes it.
func NewModule(
	router *gas.Router,
	logger gas.Logger,
	storage contracts.StorageProvider,
	verifier *sigv4.Verifier,
	az *authz.Authorizer,
) *Module {
	return &Module{
		router:   router,
		logger:   logger.With().Str("module", moduleName).Logger(),
		storage:  storage,
		verifier: verifier,
		authz:    az,
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
	// Keys may contain slashes, so they are matched with a wildcard. GET must
	// be registered before HEAD: the router maps HEAD onto every GET route.
	const p = "/{bucket}/*"
	m.router.Handle(m, http.MethodGet, p, m.getObject)   // Download an object
	m.router.Handle(m, http.MethodHead, p, m.headObject) // Show object metadata
	m.router.Handle(m, http.MethodPut, p, m.putObject)   // Upload an object, a copy, or a part
	m.router.Handle(
		m,
		http.MethodDelete,
		p,
		m.deleteObject,
	) // Delete an object (or abort an upload)
	m.router.Handle(m, http.MethodPost, p, m.postObject) // Start or complete a multipart upload
}
