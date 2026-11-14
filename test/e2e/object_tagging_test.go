//go:build e2e

package e2e

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The tests here run the live server and check object tagging: the tagging
// operations, the tags every write path stores, the count reads answer,
// and S3's tag limits.

// requireObjectTags fails unless the object at key in bucket "tags" has
// want as its tags, "k=v" each, in order, and a read and a HEAD answer its
// count, none for no tags.
func requireObjectTags(t *testing.T, c *s3.Client, key string, want ...string) {
	t.Helper()
	ctx := t.Context()
	out, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String(key),
	})
	requireNoErr(t, err, "get the tags of "+key)
	got := make([]string, len(out.TagSet))
	for i, tag := range out.TagSet {
		got[i] = aws.ToString(tag.Key) + "=" + aws.ToString(tag.Value)
	}
	requireEqual(t, strings.Join(got, "&"), strings.Join(want, "&"), "tags of "+key)

	var count *int32
	if len(want) > 0 {
		count = aws.Int32(int32(len(want))) //nolint:gosec // at most 10 tags
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("tags"), Key: aws.String(key),
	})
	requireNoErr(t, err, "head "+key)
	requireEqual(t, aws.ToInt32(head.TagCount), aws.ToInt32(count), "HEAD tag count of "+key)
	requireEqual(t, head.TagCount == nil, count == nil, "HEAD tag count present for "+key)
	getOut, _ := get(t, c, &s3.GetObjectInput{Bucket: aws.String("tags"), Key: aws.String(key)})
	requireEqual(t, aws.ToInt32(getOut.TagCount), aws.ToInt32(count), "GET tag count of "+key)
	requireEqual(t, getOut.TagCount == nil, count == nil, "GET tag count present for "+key)
}

// putTagged writes key to bucket "tags" with the x-amz-tagging header
// tagging.
func putTagged(t *testing.T, c *s3.Client, key, tagging string) {
	t.Helper()
	_, err := c.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("tags"), Key: aws.String(key), Body: strings.NewReader(key),
		Tagging: aws.String(tagging),
	})
	requireNoErr(t, err, "put "+key+" with tags")
}

func TestObjectTaggingOperations(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "tags")

	put(t, c, "tags", "k", []byte("k"))
	requireObjectTags(t, c, "k")
	_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String("k"),
		Tagging: tags("team", "storage", "note", "a b/c:d=e+f@g_h.i-j"),
	})
	requireNoErr(t, err, "put object tagging")
	requireObjectTags(t, c, "k", "team=storage", "note=a b/c:d=e+f@g_h.i-j")

	// A tag set replaces the whole set; an empty one removes it.
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String("k"), Tagging: tags("only", ""),
	})
	requireNoErr(t, err, "replace the tags")
	requireObjectTags(t, c, "k", "only=")
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String("k"),
		Tagging: &types.Tagging{TagSet: []types.Tag{}},
	})
	requireNoErr(t, err, "put an empty tag set")
	requireObjectTags(t, c, "k")

	putTagged(t, c, "k", "a=1")
	_, err = c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String("k"),
	})
	requireNoErr(t, err, "delete object tagging")
	requireObjectTags(t, c, "k")

	for name, call := range map[string]func() error{
		"get": func() error {
			_, e := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
				Bucket: aws.String("tags"), Key: aws.String("missing"),
			})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"put": func() error {
			_, e := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
				Bucket: aws.String("tags"), Key: aws.String("missing"), Tagging: tags("a", "1"),
			})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
		"delete": func() error {
			_, e := c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
				Bucket: aws.String("tags"), Key: aws.String("missing"),
			})
			return e //nolint:wrapcheck // the test inspects the SDK error
		},
	} {
		t.Run(name+" missing", func(t *testing.T) { requireCode(t, call(), "NoSuchKey") })
	}
}

// Every write stores its own request's tags, and none without: PutObject,
// CreateMultipartUpload and CopyObject, under each tagging directive. A
// write over a tagged object without tags leaves it untagged.
func TestObjectTaggingOnWrites(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "tags")

	putTagged(t, c, "src", "team=storage&env=prod%20eu")
	requireObjectTags(t, c, "src", "team=storage", "env=prod eu")
	put(t, c, "tags", "over", []byte("x"))
	putTagged(t, c, "over", "a=1")
	put(t, c, "tags", "over", []byte("y"))
	requireObjectTags(t, c, "over")

	m, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("tags"), Key: aws.String("mpu"), Tagging: aws.String("upload=yes"),
	})
	requireNoErr(t, err, "create a tagged upload")
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("tags"), Key: aws.String("mpu"), UploadId: m.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("part")),
	})
	requireNoErr(t, err, "upload a part")
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("tags"), Key: aws.String("mpu"), UploadId: m.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.ETag},
		}},
	})
	requireNoErr(t, err, "complete the upload")
	requireObjectTags(t, c, "mpu", "upload=yes")

	requireCopiesTags(t, c)
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("tags"), Key: aws.String("x"), CopySource: copySource("tags", "src"),
		TaggingDirective: "MERGE",
	})
	requireCode(t, err, "InvalidArgument")

	// A deleted object's tags do not come back with a new object.
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("tags"), Key: aws.String("src"),
	})
	requireNoErr(t, err, "delete src")
	put(t, c, "tags", "src", []byte("new"))
	requireObjectTags(t, c, "src")
}

// S3's object tag limits, on PutObjectTagging and on the x-amz-tagging
// header alike.
func TestObjectTagLimits(t *testing.T) {
	l := startLive(t, nil)
	c := l.testClient()
	ctx := t.Context()
	createBucket(t, c, "tags")
	put(t, c, "tags", "k", []byte("k"))

	var ten, eleven []string
	for i := range 11 {
		kv := []string{string(rune('a' + i)), "v"}
		if i < 10 {
			ten = append(ten, kv...)
		}
		eleven = append(eleven, kv...)
	}
	_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String("k"), Tagging: tags(ten...),
	})
	requireNoErr(t, err, "ten tags")
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("tags"), Key: aws.String("k"),
		Tagging: tags(strings.Repeat("é", 128), strings.Repeat("ü", 256)),
	})
	requireNoErr(t, err, "the longest key and value")

	for name, tagging := range map[string]*types.Tagging{
		"eleven tags":     tags(eleven...),
		"a long key":      tags(strings.Repeat("k", 129), "v"),
		"a long value":    tags("k", strings.Repeat("v", 257)),
		"an empty key":    tags("", "v"),
		"a reserved key":  tags("AWS:team", "v"),
		"a bad character": tags("team", "a*b"),
		"a duplicate key": tags("team", "a", "team", "b"),
	} {
		t.Run(name, func(t *testing.T) {
			_, pErr := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
				Bucket: aws.String("tags"), Key: aws.String("k"), Tagging: tagging,
			})
			requireCode(t, pErr, "InvalidTag")
		})
	}

	for header, code := range map[string]string{
		"a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8&i=9&j=10&k=11": "InvalidTag",
		"aws%3Ateam=v":  "InvalidTag",
		"team=a&team=b": "InvalidArgument",
		"team=%zz":      "InvalidArgument",
	} {
		_, err = c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("tags"), Key: aws.String("h"), Body: strings.NewReader("h"),
			Tagging: aws.String(header),
		})
		requireCode(t, err, code)
	}
	requireAbsent(t, c, "tags", "h")
}

// requireCopiesTags copies "src" of bucket "tags", whose tags are
// team=storage and env=prod eu, under each tagging directive, and fails
// unless each copy has the tags S3 gives it.
func requireCopiesTags(t *testing.T, c *s3.Client) {
	t.Helper()
	for _, tt := range []struct {
		directive types.TaggingDirective
		tagging   *string
		key       string
		want      []string
	}{
		{key: "copy-default", want: []string{"team=storage", "env=prod eu"}},
		// Under COPY the header is not read, so one that does not decode
		// is no error.
		{
			key: "copy-unread", tagging: aws.String("a=%zz"),
			want: []string{"team=storage", "env=prod eu"},
		},
		{
			key: "copy-copy", directive: types.TaggingDirectiveCopy, tagging: aws.String("x=1"),
			want: []string{"team=storage", "env=prod eu"},
		},
		{
			key: "copy-replace", directive: types.TaggingDirectiveReplace,
			tagging: aws.String("x=1"), want: []string{"x=1"},
		},
		{key: "copy-replace-none", directive: types.TaggingDirectiveReplace},
	} {
		_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
			Bucket:           aws.String("tags"),
			Key:              aws.String(tt.key),
			CopySource:       copySource("tags", "src"),
			TaggingDirective: tt.directive,
			Tagging:          tt.tagging,
		})
		requireNoErr(t, err, "copy to "+tt.key)
		requireObjectTags(t, c, tt.key, tt.want...)
	}
}
