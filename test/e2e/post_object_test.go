//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // an ETag is the MD5 of the object
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	mimemultipart "mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// pdfConditions are the conditions a server that presigns PDF uploads adds
// to the policy PresignPostObject signs.
var pdfConditions = []any{
	[]any{"content-length-range", 1, 100000000},
	map[string]string{"Content-Type": "application/pdf"},
}

// presignPost presigns a POST Object of key to the docs bucket with c, the
// policy carrying conds besides the SDK's own.
func presignPost(t *testing.T, c *s3.Client, key string, conds ...any) *s3.PresignedPostRequest {
	t.Helper()
	pre, err := s3.NewPresignClient(c).PresignPostObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String(key),
	}, func(o *s3.PresignPostOptions) { o.Conditions = conds })
	requireNoErr(t, err, "presign post")
	return pre
}

// postResponse is a POST Object's answer.
type postResponse struct {
	header http.Header
	err    s3Error
	status int
}

// postForm posts fields, in name order and then extra in its order, and
// then file, named filename, to url as a multipart/form-data form.
func postForm(
	t *testing.T, client *http.Client, url string,
	fields map[string]string, extra [][2]string, filename string, file []byte,
) postResponse {
	t.Helper()
	return postFormTo(t, client, url, "", fields, extra, filename, file)
}

// postFormTo is postForm with the request's Host set to host, unless "".
func postFormTo(
	t *testing.T, client *http.Client, url, host string,
	fields map[string]string, extra [][2]string, filename string, file []byte,
) postResponse {
	t.Helper()
	var body bytes.Buffer
	mw := mimemultipart.NewWriter(&body)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		requireNoErr(t, mw.WriteField(name, fields[name]), "write field "+name)
	}
	for _, f := range extra {
		requireNoErr(t, mw.WriteField(f[0], f[1]), "write field "+f[0])
	}
	fw, err := mw.CreateFormFile("file", filename)
	requireNoErr(t, err, "create file part")
	_, err = fw.Write(file)
	requireNoErr(t, err, "write file part")
	requireNoErr(t, mw.Close(), "close form")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, &body)
	requireNoErr(t, err, "build post")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if host != "" {
		req.Host = host
	}
	res, err := client.Do(req)
	requireNoErr(t, err, "post form")
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	requireNoErr(t, err, "read post response")
	out := postResponse{status: res.StatusCode, header: res.Header}
	if res.StatusCode >= 300 {
		requireNoErr(t, xml.Unmarshal(b, &out.err), "decode error "+string(b))
	} else {
		requireEqual(t, len(b), 0, "success body length")
	}
	return out
}

// withField returns fields with name set to value.
func withField(fields map[string]string, name, value string) map[string]string {
	out := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		out[k] = v
	}
	out[name] = value
	return out
}

// requireContentType checks the content type key in bucket was stored with.
func requireContentType(t *testing.T, c *s3.Client, bucket, key, want string) {
	t.Helper()
	out, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	requireNoErr(t, err, "head "+key)
	requireEqual(t, aws.ToString(out.ContentType), want, "content type of "+key)
}

// pdf is the file the POST Object tests post.
var pdf = []byte("%PDF-1.7 the converted document")

// startPostServer starts a server with the docs bucket, and returns it and
// a client of it.
func startPostServer(t *testing.T) (*liveServer, *s3.Client) {
	t.Helper()
	srv := startLive(t, nil)
	c := clientFor(srv)
	createBucket(t, c, "docs")
	return srv, c
}

// TestPostObject checks a POST Object form as the AWS SDK's
// PresignPostObject signs it, with conditions on its size and type: one within
// the policy is stored, and one too large or empty is refused and stores
// nothing, leaving an object already at the key as it was.
func TestPostObject(t *testing.T) {
	srv, c := startPostServer(t)
	pre := presignPost(t, c, "doc.pdf", pdfConditions...)
	requireEqual(t, pre.URL, srv.URL+"/docs", "post URL")
	fields := withField(pre.Values, "Content-Type", "application/pdf")

	t.Run("within the policy", func(t *testing.T) {
		res := postForm(t, srv.Client(), pre.URL, fields, nil, "doc.pdf", pdf)
		requireEqual(t, res.status, http.StatusNoContent, "status")
		sum := md5.Sum(pdf) //nolint:gosec // an ETag is the MD5 of the object
		requireEqual(t, res.header.Get("ETag"), `"`+hex.EncodeToString(sum[:])+`"`, "etag")
		requireBody(t, c, "docs", "doc.pdf", string(pdf))
		requireContentType(t, c, "docs", "doc.pdf", "application/pdf")
	})

	small := presignPost(t, c, "small.pdf",
		[]any{"content-length-range", 1, 10},
		map[string]string{"Content-Type": "application/pdf"})
	smallFields := withField(small.Values, "Content-Type", "application/pdf")

	t.Run("too large", func(t *testing.T) {
		res := postForm(t, srv.Client(), small.URL, smallFields, nil, "f", []byte("eleven byte"))
		requireS3Error(t, res.status, res.err, http.StatusBadRequest, "EntityTooLarge",
			"Your proposed upload exceeds the maximum allowed object size.")
		requireAbsent(t, c, "docs", "small.pdf")

		// An object already at the key is left as it was.
		put(t, c, "docs", "small.pdf", []byte("kept"))
		res = postForm(t, srv.Client(), small.URL, smallFields, nil, "f",
			bytes.Repeat([]byte("x"), 1<<20))
		requireS3Error(t, res.status, res.err, http.StatusBadRequest, "EntityTooLarge", "")
		requireBody(t, c, "docs", "small.pdf", "kept")
	})

	t.Run("empty", func(t *testing.T) {
		res := postForm(t, srv.Client(), pre.URL, fields, nil, "doc.pdf", nil)
		requireS3Error(t, res.status, res.err, http.StatusBadRequest, "EntityTooSmall",
			"Your proposed upload is smaller than the minimum allowed object size.")
		requireBody(t, c, "docs", "doc.pdf", string(pdf))
	})
}

// TestPostObjectOutsideThePolicy checks that a form of another content type
// or key, or with a field the policy does not cover, is refused and stores
// nothing.
func TestPostObjectOutsideThePolicy(t *testing.T) {
	srv, c := startPostServer(t)
	empty := presignPost(t, c, "empty.pdf", pdfConditions...)
	emptyFields := withField(empty.Values, "Content-Type", "application/pdf")

	t.Run("wrong content type", func(t *testing.T) {
		res := postForm(t, srv.Client(), empty.URL,
			withField(emptyFields, "Content-Type", "text/plain"), nil, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "AccessDenied",
			`Invalid according to Policy: Policy Condition failed: `+
				`{"Content-Type":"application/pdf"}`)
		requireAbsent(t, c, "docs", "empty.pdf")
	})

	t.Run("another key", func(t *testing.T) {
		res := postForm(t, srv.Client(), empty.URL,
			withField(emptyFields, "key", "other.pdf"), nil, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "AccessDenied",
			`Invalid according to Policy: Policy Condition failed: {"key":"empty.pdf"}`)
		requireAbsent(t, c, "docs", "other.pdf")
		requireAbsent(t, c, "docs", "empty.pdf")
	})

	t.Run("a field the policy does not cover", func(t *testing.T) {
		res := postForm(t, srv.Client(), empty.URL, emptyFields,
			[][2]string{{"x-amz-meta-owner", "mallory"}}, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "AccessDenied",
			"Invalid according to Policy: Extra input fields: x-amz-meta-owner")
		requireAbsent(t, c, "docs", "empty.pdf")
	})
}

// TestPostObjectAuthentication checks that a form whose policy expired, is
// badly signed or is signed with a key the server does not know is refused
// and stores nothing.
func TestPostObjectAuthentication(t *testing.T) {
	srv, c := startPostServer(t)
	empty := presignPost(t, c, "empty.pdf", pdfConditions...)
	emptyFields := withField(empty.Values, "Content-Type", "application/pdf")

	t.Run("expired policy", func(t *testing.T) {
		expired := signPolicy(t, testCreds, time.Now().Add(-time.Minute), []any{
			map[string]string{"bucket": "docs"},
			map[string]string{"key": "empty.pdf"},
		})
		res := postForm(t, srv.Client(), srv.URL+"/docs", expired, nil, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "AccessDenied",
			"Invalid according to Policy: Policy expired.")
		requireAbsent(t, c, "docs", "empty.pdf")
	})

	t.Run("bad signature", func(t *testing.T) {
		sig := emptyFields["X-Amz-Signature"]
		forged := withField(emptyFields, "X-Amz-Signature", flipLastHex(sig))
		res := postForm(t, srv.Client(), empty.URL, forged, nil, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "SignatureDoesNotMatch",
			"The request signature we calculated does not match the signature you provided. "+
				"Check your key and signing method.")
		requireAbsent(t, c, "docs", "empty.pdf")

		// A policy changed after it was signed does not verify either.
		policy, err := base64.StdEncoding.DecodeString(emptyFields["policy"])
		requireNoErr(t, err, "decode policy")
		widened := strings.Replace(string(policy), `{"key":"empty.pdf"}`,
			`["starts-with","$key",""]`, 1)
		res = postForm(t, srv.Client(), empty.URL,
			withField(withField(emptyFields, "policy",
				base64.StdEncoding.EncodeToString([]byte(widened))), "key", "any.pdf"),
			nil, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "SignatureDoesNotMatch", "")
		requireAbsent(t, c, "docs", "any.pdf")
	})

	t.Run("an unknown key", func(t *testing.T) {
		other := signPolicy(t, aws.Credentials{AccessKeyID: "nobody", SecretAccessKey: "x"},
			time.Now().Add(time.Hour), []any{
				map[string]string{"bucket": "docs"},
				map[string]string{"key": "empty.pdf"},
			})
		res := postForm(t, srv.Client(), srv.URL+"/docs", other, nil, "f", pdf)
		requireS3Error(t, res.status, res.err, http.StatusForbidden, "InvalidAccessKeyId", "")
		requireAbsent(t, c, "docs", "empty.pdf")
	})
}

// TestPostObjectConditions checks the other condition forms: starts-with
// on the key, which ${filename} expands before it is checked, eq as an
// array, success_action_status 200 and x-ignore- fields; and that a field
// the policy covers but the server does not act on is refused rather than
// dropped.
func TestPostObjectConditions(t *testing.T) {
	srv := startLive(t, nil)
	c := clientFor(srv)
	createBucket(t, c, "docs")

	pre := presignPost(t, c, "uploads/${filename}",
		[]any{"starts-with", "$key", "uploads/"},
		[]any{"eq", "$Content-Type", "application/pdf"},
		map[string]string{"success_action_status": "200"},
	)
	fields := withField(pre.Values, "Content-Type", "application/pdf")
	fields["success_action_status"] = "200"
	res := postForm(t, srv.Client(), pre.URL, fields,
		[][2]string{{"x-ignore-submit", "Upload"}}, "report.pdf", []byte("report"))
	requireEqual(t, res.status, http.StatusOK, "status")
	requireBody(t, c, "docs", "uploads/report.pdf", "report")

	res = postForm(t, srv.Client(), pre.URL, withField(fields, "key", "elsewhere/x.pdf"), nil,
		"x.pdf", []byte("x"))
	requireS3Error(t, res.status, res.err, http.StatusForbidden, "AccessDenied",
		`Invalid according to Policy: Policy Condition failed: ["starts-with","$key","uploads/"]`)
	requireAbsent(t, c, "docs", "elsewhere/x.pdf")

	acl := presignPost(t, c, "acl.pdf", map[string]string{"acl": "private"})
	res = postForm(t, srv.Client(), acl.URL, withField(acl.Values, "acl", "private"), nil,
		"f", []byte("x"))
	requireS3Error(t, res.status, res.err, http.StatusNotImplemented, "NotImplemented",
		"The POST form field acl is not supported.")
	requireAbsent(t, c, "docs", "acl.pdf")
}

// TestPostObjectVirtualHost checks a form posted to <bucket>.<domain>/, as
// the SDK addresses it without path-style.
func TestPostObjectVirtualHost(t *testing.T) {
	srv := startLive(t, map[string]string{"TULE_Server_Domain": "s3.test"})
	c := clientFor(srv)
	createBucket(t, c, "docs")
	pre := presignPost(t, c, "vh.pdf")
	res := postFormTo(t, srv.Client(), srv.URL+"/", "docs.s3.test", pre.Values, nil,
		"f", []byte("hosted"))
	requireEqual(t, res.status, http.StatusNoContent, "status")
	requireBody(t, c, "docs", "vh.pdf", "hosted")
}

// signPolicy returns the fields of a POST Object form signed with creds for
// a policy expiring at exp with conds, as PresignPostObject signs one, for
// the policies it cannot make.
func signPolicy(
	t *testing.T, creds aws.Credentials, exp time.Time, conds []any,
) map[string]string {
	t.Helper()
	now := time.Now().UTC()
	cred := creds.AccessKeyID + "/" + now.Format("20060102") + "/us-east-1/s3/aws4_request"
	date := now.Format(amzDate)
	conds = append([]any{
		map[string]string{"X-Amz-Algorithm": "AWS4-HMAC-SHA256"},
		map[string]string{"X-Amz-Credential": cred},
		map[string]string{"X-Amz-Date": date},
	}, conds...)
	doc, err := json.Marshal(map[string]any{
		"expiration": exp.UTC().Format(time.RFC3339), "conditions": conds,
	})
	requireNoErr(t, err, "encode policy")
	encoded := base64.StdEncoding.EncodeToString(doc)

	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+creds.SecretAccessKey), now.Format("20060102"))
	k = mac(mac(mac(k, "us-east-1"), "s3"), "aws4_request")

	fields := map[string]string{
		"X-Amz-Algorithm":  "AWS4-HMAC-SHA256",
		"X-Amz-Credential": cred,
		"X-Amz-Date":       date,
		"X-Amz-Signature":  hex.EncodeToString(mac(k, encoded)),
		"policy":           encoded,
	}
	for _, c := range conds {
		if m, ok := c.(map[string]string); ok {
			if v, ok := m["key"]; ok {
				fields["key"] = v
			}
		}
	}
	return fields
}

// flipLastHex returns sig, hex, with its last digit changed.
func flipLastHex(sig string) string {
	repl := "0"
	if strings.HasSuffix(sig, "0") {
		repl = "1"
	}
	return sig[:len(sig)-1] + repl
}

// TestPostObjectIdentity checks that a POST Object form is evaluated as
// s3:PutObject for the IAM user whose key signed its policy, with
// s3:authType POST: a user allowed s3:PutObject only for POST may post a
// form but not put the object, and a user no policy allows is refused the
// form.
func TestPostObjectIdentity(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	srv := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	root := s3As(srv, rootCreds())
	createBucket(t, root, "docs")

	poster := s3As(srv, createUserKey(t, ic, "poster"))
	putUserPolicy(t, ic, "poster", "post-only", `{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::docs/*",`+
		`"Condition":{"StringEquals":{"s3:authType":"POST"}}}]}`)
	pre := presignPost(t, poster, "posted.pdf")
	res := postForm(t, srv.Client(), pre.URL, pre.Values, nil, "f", pdf)
	requireEqual(t, res.status, http.StatusNoContent, "posted by a user allowed POST")
	requireBody(t, root, "docs", "posted.pdf", string(pdf))
	_, err := poster.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("put.pdf"), Body: bytes.NewReader(pdf),
	})
	requireCode(t, err, "AccessDenied")

	nobody := s3As(srv, createUserKey(t, ic, "nobody"))
	pre = presignPost(t, nobody, "denied.pdf")
	res = postForm(t, srv.Client(), pre.URL, pre.Values, nil, "f", pdf)
	requireS3Error(t, res.status, res.err, http.StatusForbidden, "AccessDenied",
		notAllowed("arn:aws:iam::000000000000:user/nobody", "s3:PutObject",
			"arn:aws:s3:::docs/denied.pdf"))
	requireAbsent(t, root, "docs", "denied.pdf")
}

// TestPostObjectVersioned checks that in a bucket with versioning Enabled a
// stored form is a new version, answered with its x-amz-version-id, and a
// refused one leaves no version behind.
func TestPostObjectVersioned(t *testing.T) {
	srv, c := startPostServer(t)
	setVersioning(t, c, "docs", types.BucketVersioningStatusEnabled)

	small := presignPost(t, c, "v.pdf", []any{"content-length-range", 1, 10})
	res := postForm(t, srv.Client(), small.URL, small.Values, nil, "f", []byte("eleven byte"))
	requireS3Error(t, res.status, res.err, http.StatusBadRequest, "EntityTooLarge", "")
	listed, err := c.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
		Bucket: aws.String("docs"),
	})
	requireNoErr(t, err, "list versions")
	requireEqual(t, len(listed.Versions)+len(listed.DeleteMarkers), 0, "versions after a refusal")

	res = postForm(t, srv.Client(), small.URL, small.Values, nil, "f", []byte("ten bytes!"))
	requireEqual(t, res.status, http.StatusNoContent, "status")
	listed, err = c.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
		Bucket: aws.String("docs"),
	})
	requireNoErr(t, err, "list versions")
	requireEqual(t, len(listed.Versions), 1, "versions after a stored form")
	requireEqual(t, res.header.Get("X-Amz-Version-Id"), aws.ToString(listed.Versions[0].VersionId),
		"x-amz-version-id")
}

// postSourcePDF posts fields, in name order, then file as a server-side
// uploader sends it, a part named source.pdf of type application/pdf, to url. The
// body is sent with its Content-Length, or chunked, with none.
func postSourcePDF(
	t *testing.T, client *http.Client, url string, fields map[string]string, file []byte,
	chunked bool,
) postResponse {
	t.Helper()
	var body bytes.Buffer
	mw := mimemultipart.NewWriter(&body)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		requireNoErr(t, mw.WriteField(name, fields[name]), "write field "+name)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="source.pdf"`)
	h.Set("Content-Type", "application/pdf")
	fw, err := mw.CreatePart(h)
	requireNoErr(t, err, "create file part")
	_, err = fw.Write(file)
	requireNoErr(t, err, "write file part")
	requireNoErr(t, mw.Close(), "close form")

	var r io.Reader = &body
	if chunked {
		// A reader of unknown length is sent chunked.
		r = io.MultiReader(&body)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, r)
	requireNoErr(t, err, "build post")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := client.Do(req)
	requireNoErr(t, err, "post form")
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	requireNoErr(t, err, "read post response")
	out := postResponse{status: res.StatusCode, header: res.Header}
	if res.StatusCode >= 300 {
		requireNoErr(t, xml.Unmarshal(b, &out.err), "decode error "+string(b))
	}
	return out
}

// TestPostObjectWithSessionCredentials checks a form signed with a role
// session's credentials, as a function running with temporary credentials
// signs one: its X-Amz-Security-Token is checked with the key, and the file
// is stored whether it is sent with a Content-Length or chunked. A token
// that is not the session's is refused.
func TestPostObjectWithSessionCredentials(t *testing.T) {
	identity := startSibling(t, "identity", identityDirEnv)
	srv := startLiveWithIdentity(t, identity.URL)
	ic := iamClient(identity.URL)
	root := s3As(srv, rootCreds())
	createBucket(t, root, "docs")

	creds := uploaderSession(t, identity.URL, ic)
	session := s3As(srv, creds)

	for _, chunked := range []bool{false, true} {
		k := fmt.Sprintf("previews/job-chunked-%t.pdf", chunked)
		pre := presignPost(t, session, k, pdfConditions...)
		requireEqual(t, pre.Values["X-Amz-Security-Token"],
			creds.SessionToken, "token field")
		fields := withField(pre.Values, "Content-Type", "application/pdf")
		res := postSourcePDF(t, srv.Client(), pre.URL, fields, pdf, chunked)
		requireEqual(
			t,
			res.status,
			http.StatusNoContent,
			fmt.Sprintf("status, chunked %t", chunked),
		)
		requireBody(t, root, "docs", k, string(pdf))
		requireContentType(t, root, "docs", k, "application/pdf")
	}

	pre := presignPost(t, session, "previews/forged.pdf", pdfConditions...)
	fields := withField(pre.Values, "Content-Type", "application/pdf")
	fields["X-Amz-Security-Token"] = flipLast(fields["X-Amz-Security-Token"])
	res := postSourcePDF(t, srv.Client(), pre.URL, fields, pdf, false)
	requireS3Error(t, res.status, res.err, http.StatusBadRequest, "InvalidToken",
		"The provided token is malformed or otherwise invalid.")
	requireAbsent(t, root, "docs", "previews/forged.pdf")
}

// uploaderSession returns the credentials of a session of the role
// Uploader, allowed s3:PutObject in docs, assumed by the IAM user dev.
func uploaderSession(t *testing.T, identityURL string, ic *iam.Client) aws.Credentials {
	t.Helper()
	ctx := t.Context()
	key := createUserKey(t, ic, "dev")
	_, err := ic.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String("Uploader"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[` +
			`{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:user/dev"},` +
			`"Action":"sts:AssumeRole"}]}`),
	})
	requireNoErr(t, err, "create role")
	_, err = ic.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName: aws.String("Uploader"), PolicyName: aws.String("previews"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":"s3:PutObject","Resource":"arn:aws:s3:::docs/*"}]}`),
	})
	requireNoErr(t, err, "put role policy")
	sc := sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(identityURL),
		Credentials:      credentials.StaticCredentialsProvider{Value: key},
		HTTPClient:       &http.Client{Timeout: serviceTimeout},
		RetryMaxAttempts: 1,
	})
	assumed, err := sc.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String("arn:aws:iam::000000000000:role/Uploader"),
		RoleSessionName: aws.String("uploader"),
	})
	requireNoErr(t, err, "assume role")
	return aws.Credentials{
		AccessKeyID:     aws.ToString(assumed.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(assumed.Credentials.SessionToken),
	}
}
