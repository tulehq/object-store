package s3api

import (
	"errors"
	"io"
	"strings"
	"testing"
)

var chunkSig = ";chunk-signature=" + strings.Repeat("a", 64)

var chunkedTests = []struct {
	name    string
	body    string
	want    string
	wantLen int64
	wantErr bool
}{
	{
		name: "unsigned with trailer",
		body: "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n",
		want: "hello world", wantLen: 11,
	},
	{
		name: "signed chunks",
		body: "5" + chunkSig + "\r\nhello\r\n0" + chunkSig + "\r\n\r\n",
		want: "hello", wantLen: 5,
	},
	{
		name: "unknown decoded length",
		body: "3\r\nabc\r\n0\r\n\r\n",
		want: "abc", wantLen: -1,
	},
	{
		name:    "decoded length mismatch",
		body:    "3\r\nabc\r\n0\r\n\r\n",
		wantLen: 4, wantErr: true,
	},
	{
		name:    "truncated chunk data",
		body:    "a\r\nabc",
		wantLen: -1, wantErr: true,
	},
	{
		name:    "missing final chunk",
		body:    "3\r\nabc\r\n",
		wantLen: -1, wantErr: true,
	},
	{
		name:    "chunk longer than declared",
		body:    "3\r\nabcd\r\n0\r\n\r\n",
		wantLen: -1, wantErr: true,
	},
	{
		name:    "bad chunk size",
		body:    "zz\r\nabc\r\n",
		wantLen: -1, wantErr: true,
	},
}

func TestChunkedReader(t *testing.T) {
	for _, tt := range chunkedTests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := io.ReadAll(NewChunkedReader(strings.NewReader(tt.body), tt.wantLen))
			if tt.wantErr {
				var s3Err *Error
				if !errors.As(err, &s3Err) && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("want decode error, got %v (data %q)", err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
