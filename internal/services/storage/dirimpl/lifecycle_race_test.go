package dirimpl

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gasmod/gas/storage"

	"tule.dev/object-store/internal/contracts"
)

// A versioned write whose data is written while ApplyLifecycle holds the
// key commits on top once it is done: the decision applies to the key as
// it was read, and the new version is current and whole.
func TestApplyLifecycleRacingAVersionedWrite(t *testing.T) {
	f, _ := newClockFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	written := make(chan struct{})
	put := make(chan *contracts.ObjectInfo, 1)
	var once sync.Once
	f.s.hook = func(stage string) error {
		switch stage {
		case stageLifecycleDecided:
			once.Do(func() {
				go func() {
					info, err := f.s.PutObject(f.ctx, "bucket", "k", strings.NewReader("new"),
						contracts.PutObjectInput{}, contracts.Precondition{})
					if err != nil {
						t.Error(err)
					}
					put <- info
				}()
				// The write's data is written; its commit waits for the
				// key's lock.
				<-written
			})
		case stageWritten:
			close(written)
		}
		return nil
	}
	out := f.apply(planOf(expire))
	info := <-put
	f.s.hook = nil

	f.requireIDs(info.VersionID, out.MarkerID, v1.VersionID)
	if f.body(oref("bucket", "k")) != "new" || f.body(vref("k", v1.VersionID)) != "v1" {
		t.Fatal("the new version is not current and whole, or the expired one not kept")
	}
	f.requireSince(time.Time{}, info.LastModified, midnight)
}

// blockingReader blocks its first Read until release is closed, after
// closing reading.
type blockingReader struct {
	r       io.Reader
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.reading)
		<-b.release
	})
	return b.r.Read(p) //nolint:wrapcheck // io.Reader contract: EOF must pass through unwrapped
}

// A write in place holds the key's lock while it reads its body, so
// ApplyLifecycle finds the key busy rather than waiting for it.
func TestApplyLifecycleRacingALegacyWrite(t *testing.T) {
	f := newFixture(t)
	f.put("k", "old")
	body := &blockingReader{
		r: strings.NewReader("new"), reading: make(chan struct{}), release: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- errOf(f.s.PutObject(f.ctx, "bucket", "k", body, contracts.PutObjectInput{},
			contracts.Precondition{}))
	}()
	<-body.reading
	d := planOf(expire)
	out := f.apply(d)
	close(body.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !out.Busy || len(d.keys) != 0 || f.body(oref("bucket", "k")) != "new" {
		t.Fatalf("outcome %+v", out)
	}
}

// Tags read for a rule are the version's tags before or after a retag that
// races it, never part of each.
func TestApplyLifecycleRacingARetag(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	sets := [][]contracts.Tag{
		{{Key: "a", Value: "1"}, {Key: "b", Value: "1"}},
		{{Key: "a", Value: "2"}, {Key: "b", Value: "2"}},
	}
	stop, retagged := make(chan struct{}), make(chan error, 1)
	go retagLoop(f, sets, stop, retagged)
	for range 200 {
		f.apply(&stubDecider{plan: untornTags(t)})
	}
	close(stop)
	if err := <-retagged; err != nil {
		t.Fatal(err)
	}
}

// retagLoop sets the tags of "k" to each of sets in turn until stop is
// closed, then sends nil to done, or the error that stopped it.
func retagLoop(f *fixture, sets [][]contracts.Tag, stop <-chan struct{}, done chan<- error) {
	for i := 0; ; i++ {
		select {
		case <-stop:
			done <- nil
			return
		default:
		}
		_, err := f.s.PutObjectTagging(f.ctx, oref("bucket", "k"), sets[i%len(sets)], nil)
		if err != nil {
			done <- err
			return
		}
	}
}

// untornTags is a plan that fails the test unless the current version's
// tags are one of the sets retagLoop sets, or none.
func untornTags(t *testing.T) func(*contracts.LifecycleKey) (contracts.LifecyclePlan, error) {
	t.Helper()
	return func(k *contracts.LifecycleKey) (contracts.LifecyclePlan, error) {
		tags, err := k.Versions[0].Tags()
		if err != nil {
			return contracts.LifecyclePlan{}, err
		}
		if len(tags) != 0 && (len(tags) != 2 || tags[0].Value != tags[1].Value) {
			t.Errorf("torn tags %v", tags)
		}
		return contracts.LifecyclePlan{}, nil
	}
}

// A read of a noncurrent version that lifecycle rules remove meanwhile
// reads the whole version it opened, and a read after finds no version.
func TestApplyLifecycleRacingAVersionRead(t *testing.T) {
	f := newFixture(t)
	f.state(contracts.VersioningEnabled)
	body := strings.Repeat("v1", 1<<16)
	v1 := f.put("k", body)
	f.put("k", "v2")
	obj, err := f.s.GetObject(f.ctx, vref("k", v1.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	f.apply(planOf(contracts.LifecyclePlan{Remove: map[string]string{v1.VersionID: "nc"}}))
	got, err := io.ReadAll(obj.Body)
	if cErr := obj.Body.Close(); err != nil || cErr != nil {
		t.Fatal(err, cErr)
	}
	if string(got) != body {
		t.Fatalf("read %d bytes of %d", len(got), len(body))
	}
	_, err = f.s.GetObject(f.ctx, vref("k", v1.VersionID))
	requireIs(t, "a read after", err, storage.ErrVersionNotFound)
}
