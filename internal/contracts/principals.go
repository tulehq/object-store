package contracts

import "context"

// PolicyPrincipals binds the principals of the bucket policies a server
// stores to unique IDs, and shows a bound policy as S3 shows it.
// tule.dev/identity/principals' Client is one, over identity's principal-ID
// endpoint.
type PolicyPrincipals interface {
	// Bind returns doc, a bucket policy about to be stored, with each IAM
	// user and role ARN in its principals replaced by that user's or role's
	// unique ID. An ARN no user or role has fails with an error matching
	// tule.dev/identity/principals' ErrInvalidPrincipal.
	Bind(ctx context.Context, doc string) (string, error)
	// Show returns doc, a stored bucket policy, with each unique ID that a
	// user or role still has replaced by its current ARN.
	Show(ctx context.Context, doc string) (string, error)
}
