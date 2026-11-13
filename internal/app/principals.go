package app

import (
	"context"
	"fmt"
	"net/http"

	"tule.dev/identity/principals"

	"tule.dev/object-store/internal/contracts"
)

// newPolicyPrincipals returns how bucket policies' principals are bound:
// with an identity endpoint, through its principal-ID endpoint, with the
// root key and identityTimeout per call; standalone, not at all, so
// policies are stored and returned as sent and matched by ARN.
func newPolicyPrincipals(auth *authSettings) (contracts.PolicyPrincipals, error) {
	if auth.IdentityEndpoint == "" {
		return asSent{}, nil
	}
	c, err := principals.New(principals.Config{
		HTTPClient:      &http.Client{Timeout: identityTimeout},
		Endpoint:        auth.IdentityEndpoint,
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: auth.SecretAccessKey,
	})
	if err != nil {
		return nil, fmt.Errorf("app: failed to create the principal-ID client: %w", err)
	}
	return c, nil
}

// asSent is the standalone server's PolicyPrincipals: with no identity
// there are no users or roles to bind to, so a policy is kept as sent.
type asSent struct{}

func (asSent) Bind(_ context.Context, doc string) (string, error) { return doc, nil }
func (asSent) Show(_ context.Context, doc string) (string, error) { return doc, nil }
