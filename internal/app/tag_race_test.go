package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/services/storage/dirimpl"
)

// raceStore is the app's storage with a hook run once, when armed, right
// after the next StatObjectTagging, the read authorization takes an
// object's s3:ExistingObjectTag keys from: between the evaluation and the
// handler's open of the object.
type raceStore struct {
	*dirimpl.Service
	hook func(ctx context.Context)
	mu   sync.Mutex
}

func (s *raceStore) arm(hook func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *raceStore) StatObjectTagging(
	ctx context.Context,
	bucket, key string,
) (*contracts.ObjectInfo, error) {
	info, err := s.Service.StatObjectTagging(ctx, bucket, key)
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return info, err //nolint:wrapcheck // the storage's own error, passed through
}

// replace writes body with tags over bucket/key, as another client's
// PutObject would.
func (s *raceStore) replace(
	t *testing.T,
	bucket, key, body string,
	tags []contracts.Tag,
) func(ctx context.Context) {
	return func(ctx context.Context) {
		gen, _, err := s.GetBucketPolicy(ctx, bucket)
		if err != nil && !errors.Is(err, contracts.ErrNoSuchBucketPolicy) {
			t.Error(err)
			return
		}
		pinned, release, err := s.Pin(ctx, contracts.BucketRef{Name: bucket, Generation: gen})
		if err != nil {
			t.Error(err)
			return
		}
		defer release()
		if _, err = s.PutObject(pinned, bucket, key, strings.NewReader(body),
			contracts.PutObjectInput{Tags: tags}, contracts.Precondition{}); err != nil {
			t.Error(err)
		}
	}
}

// secretDeny denies dev every action evaluated with s3:ExistingObjectTag
// on an object tagged class=secret, and allows it everything else.
const secretDeny = `{"Version":"2012-10-17","Statement":[` +
	`{"Effect":"Allow","Action":"s3:*","Resource":"*"},` +
	`{"Effect":"Deny","Action":["s3:GetObject","s3:GetObjectTagging",` +
	`"s3:GetObjectAttributes","s3:GetObjectAcl","s3:PutObjectAcl",` +
	`"s3:PutObjectTagging","s3:DeleteObjectTagging"],"Resource":"*",` +
	`"Condition":{"StringEquals":{"s3:ExistingObjectTag/class":"secret"}}}]}`

// raceCalls makes a request of every operation evaluated with
// s3:ExistingObjectTag on race/obj, or on it as a copy's source; upload is
// an upload of race/mp. GetObject fails the test when it serves anything.
func raceCalls(t *testing.T, upload *string) map[string]call {
	race, obj := aws.String("race"), aws.String("obj")
	return map[string]call{
		"GetObject": func(ctx context.Context, c *s3.Client) error {
			out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: race, Key: obj})
			if err != nil {
				return err //nolint:wrapcheck // the SDK's error, checked as is
			}
			body, _ := io.ReadAll(out.Body)
			_ = out.Body.Close()
			t.Errorf("GetObject served %q", body)
			return nil
		},
		"HeadObject": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: race, Key: obj}))
		},
		"GetObjectTagging": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.GetObjectTagging(ctx,
				&s3.GetObjectTaggingInput{Bucket: race, Key: obj}))
		},
		"GetObjectAttributes": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
				Bucket: race, Key: obj,
				ObjectAttributes: []s3types.ObjectAttributes{s3types.ObjectAttributesEtag},
			}))
		},
		"GetObjectAcl": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: race, Key: obj}))
		},
		"PutObjectAcl": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.PutObjectAcl(ctx, &s3.PutObjectAclInput{
				Bucket: race, Key: obj, ACL: s3types.ObjectCannedACLPrivate,
			}))
		},
		"PutObjectTagging": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
				Bucket: race, Key: obj, Tagging: &s3types.Tagging{TagSet: []s3types.Tag{
					{Key: aws.String("a"), Value: aws.String("b")},
				}},
			}))
		},
		"DeleteObjectTagging": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.DeleteObjectTagging(ctx,
				&s3.DeleteObjectTaggingInput{Bucket: race, Key: obj}))
		},
		"CopyObject": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket: race, Key: aws.String("dst"), CopySource: aws.String("race/obj"),
			}))
		},
		"UploadPartCopy": func(ctx context.Context, c *s3.Client) error {
			return sdkErr(c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
				Bucket: race, Key: aws.String("mp"), UploadId: upload,
				PartNumber: aws.Int32(1), CopySource: aws.String("race/obj"),
			}))
		},
	}
}

// requireUntouched fails unless the replacement of race/obj keeps its
// tags, class=secret, and nothing was copied from it, after the operation
// name.
func requireUntouched(t *testing.T, root *s3.Client, upload *string, name string) {
	t.Helper()
	ctx := t.Context()
	race := aws.String("race")
	got, err := root.GetObjectTagging(ctx,
		&s3.GetObjectTaggingInput{Bucket: race, Key: aws.String("obj")})
	if err != nil || len(got.TagSet) != 1 || aws.ToString(got.TagSet[0].Value) != "secret" {
		t.Errorf("%s: the replacement's tags are %+v, %v", name, got, err)
	}
	if _, err = root.HeadObject(ctx,
		&s3.HeadObjectInput{Bucket: race, Key: aws.String("dst")}); err == nil {
		t.Errorf("%s: the replacement was copied", name)
	}
	parts, err := root.ListParts(ctx, &s3.ListPartsInput{
		Bucket: race, Key: aws.String("mp"), UploadId: upload,
	})
	if err != nil || len(parts.Parts) != 0 {
		t.Errorf("%s: the replacement was copied into a part: %v, %v", name, parts, err)
	}
}

// A decision made on an object's tags is not applied to an object written
// at its key between the evaluation and the operation. The object is
// untagged when evaluated, so dev is allowed, and is replaced before the
// handler opens it with one tagged class=secret, which dev is denied:
// every operation evaluated with s3:ExistingObjectTag fails with 409
// OperationAborted rather than act on the replacement, and a write leaves
// it as it is. Asked again, each is denied.
func TestADecisionIsNotAppliedToAnObjectReplacedSinceEvaluation(t *testing.T) {
	f := &fakeIdentity{}
	var store *raceStore
	root, user, _ := startAuthzServerWith(t, f,
		func(s *dirimpl.Service) contracts.StorageProvider {
			store = &raceStore{Service: s}
			return store
		})
	f.setPolicies(secretDeny)
	ctx := t.Context()
	if _, err := root.CreateBucket(ctx,
		&s3.CreateBucketInput{Bucket: aws.String("race")}); err != nil {
		t.Fatal(err)
	}
	upload, err := root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("race"), Key: aws.String("mp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []contracts.Tag{{Key: "class", Value: "secret"}}
	for name, op := range raceCalls(t, upload.UploadId) {
		if _, err = root.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("race"), Key: aws.String("obj"), Body: strings.NewReader("public"),
		}); err != nil {
			t.Fatal(err)
		}
		store.arm(store.replace(t, "race", "obj", "secret", secret))
		code := "OperationAborted"
		if name == "HeadObject" {
			code = "" // a HEAD's error has no body
		}
		requireStatusIn(t, name, op(ctx, user), http.StatusConflict, code)
		requireUntouched(t, root, upload.UploadId, name)
		// Evaluated against the replacement, dev is denied.
		if denied, _ := isAccessDenied(op(ctx, user)); !denied {
			t.Errorf("%s: the replacement is not denied", name)
		}
	}
}

// requireStatusIn reports, without stopping the test, unless err is a
// response of status and, when code is set, of that error code, for the
// operation name.
func requireStatusIn(t *testing.T, name string, err error, status int, code string) {
	t.Helper()
	var respErr *awshttp.ResponseError
	var apiErr smithy.APIError
	if !errors.As(err, &respErr) || respErr.HTTPStatusCode() != status ||
		code != "" && (!errors.As(err, &apiErr) || apiErr.ErrorCode() != code) {
		t.Errorf("%s: got %v, want %d %s", name, err, status, code)
	}
}
