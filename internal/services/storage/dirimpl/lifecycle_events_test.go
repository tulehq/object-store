package dirimpl

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/notification"
)

// lifecycleEventCase is a key "k" that setup leaves, before the bucket is
// configured, the decider ApplyLifecycle is given, and the events, in
// order, that its actions record.
type lifecycleEventCase struct {
	setup   func(f *eventFixture)
	decider func() contracts.LifecycleDecider
	want    []contracts.EventName
}

// The lifecycle events.
const (
	lcDelete = contracts.EventLifecycleExpirationDelete
	lcMarker = contracts.EventLifecycleExpirationDeleteMarkerCreated
)

// soleMarker is a decider that plans p and removes a sole marker by rule
// "eodm".
func soleMarker(p contracts.LifecyclePlan) func() contracts.LifecycleDecider {
	return func() contracts.LifecycleDecider {
		d := planOf(p)
		d.sole = func(string, contracts.LifecycleVersion) (string, error) { return "eodm", nil }
		return d
	}
}

// planFor is a decider that plans what plan returns for f's "k".
func planFor(plan func(k *contracts.LifecycleKey) contracts.LifecyclePlan,
) func() contracts.LifecycleDecider {
	return func() contracts.LifecycleDecider {
		return &stubDecider{plan: func(k *contracts.LifecycleKey) (contracts.LifecyclePlan, error) {
			return plan(k), nil
		}}
	}
}

// removeNoncurrent plans to remove every noncurrent version of a key by
// rule "nc", and to expire its current one when expire is set.
func removeNoncurrent(expireCurrent bool) func(k *contracts.LifecycleKey) contracts.LifecyclePlan {
	return func(k *contracts.LifecycleKey) contracts.LifecyclePlan {
		p := contracts.LifecyclePlan{Remove: map[string]string{}}
		for _, v := range k.Versions[1:] {
			p.Remove[v.VersionID] = "nc"
		}
		if expireCurrent {
			p.ExpireCurrentRule, p.MarkerTime = "cur", midnight
		}
		return p
	}
}

// enabledWithMarker leaves "k" a version under a delete marker, in an
// Enabled bucket.
func enabledWithMarker(f *eventFixture) {
	f.state(contracts.VersioningEnabled)
	f.put("k", "v1")
	f.del(oref("bucket", "k"))
}

// suspendedOverVersion leaves "k" a legacy null version under a version
// that is not null, in a Suspended bucket.
func suspendedOverVersion(f *eventFixture) {
	f.put("k", "legacy")
	f.state(contracts.VersioningEnabled)
	f.put("k", "v2")
	f.state(contracts.VersioningSuspended)
}

// keep is a decider that does nothing, and expireOnly one that expires
// the current version.
var (
	keep       = func() contracts.LifecycleDecider { return &stubDecider{} }
	expireOnly = func() contracts.LifecycleDecider { return planOf(expire) }
)

// lifecycleEventCases are the lifecycle actions, alone and together in one
// commit, in each versioning state.
func lifecycleEventCases() map[string]lifecycleEventCase {
	cases := enabledLifecycleCases()
	maps.Copy(cases, suspendedLifecycleCases())
	cases["unversioned expiry"] = lifecycleEventCase{
		setup:   func(f *eventFixture) { f.put("k", "v1") },
		decider: expireOnly, want: []contracts.EventName{lcDelete},
	}
	cases["unversioned kept"] = lifecycleEventCase{
		setup: func(f *eventFixture) { f.put("k", "v1") }, decider: keep,
	}
	return cases
}

// enabledLifecycleCases are lifecycleEventCases' cases in an Enabled
// bucket.
func enabledLifecycleCases() map[string]lifecycleEventCase {
	return map[string]lifecycleEventCase{
		"enabled expiry": {
			setup: func(f *eventFixture) {
				f.state(contracts.VersioningEnabled)
				f.put("k", "v1")
			},
			decider: expireOnly, want: []contracts.EventName{lcMarker},
		},
		"enabled legacy key expiry": {
			setup: func(f *eventFixture) {
				f.put("k", "legacy")
				f.state(contracts.VersioningEnabled)
			},
			decider: expireOnly, want: []contracts.EventName{lcMarker},
		},
		"enabled noncurrent and expiry": {
			setup: func(f *eventFixture) {
				f.state(contracts.VersioningEnabled)
				f.put("k", "v1")
				f.put("k", "v2")
				f.put("k", "v3")
			},
			decider: planFor(removeNoncurrent(true)),
			want:    []contracts.EventName{lcDelete, lcDelete, lcMarker},
		},
		"enabled marker over a version": {
			setup: enabledWithMarker, decider: soleMarker(contracts.LifecyclePlan{}),
		},
		"enabled noncurrent then sole marker": {
			setup: enabledWithMarker,
			decider: func() contracts.LifecycleDecider {
				d := planFor(removeNoncurrent(false))().(*stubDecider)
				d.sole = func(string, contracts.LifecycleVersion) (string, error) {
					return "eodm", nil
				}
				return d
			},
			want: []contracts.EventName{lcDelete, lcDelete},
		},
		"enabled sole marker": {
			setup: func(f *eventFixture) {
				enabledWithMarker(f)
				f.del(vref("k", f.ids("k")[1]))
			},
			decider: soleMarker(contracts.LifecyclePlan{}),
			want:    []contracts.EventName{lcDelete},
		},
		"enabled kept": {
			setup: func(f *eventFixture) {
				f.state(contracts.VersioningEnabled)
				f.put("k", "v1")
			},
			decider: keep,
		},
	}
}

// suspendedLifecycleCases are lifecycleEventCases' cases in a Suspended
// bucket.
func suspendedLifecycleCases() map[string]lifecycleEventCase {
	return map[string]lifecycleEventCase{
		"suspended null current expiry": {
			setup: func(f *eventFixture) {
				f.state(contracts.VersioningEnabled)
				f.put("k", "v1")
				f.state(contracts.VersioningSuspended)
				f.put("k", "null")
			},
			decider: expireOnly, want: []contracts.EventName{lcDelete, lcMarker},
		},
		"suspended noncurrent null expiry": {
			setup: suspendedOverVersion, decider: expireOnly,
			want: []contracts.EventName{lcDelete, lcMarker},
		},
		"suspended null removed and expiry": {
			setup: suspendedOverVersion, decider: planFor(removeNoncurrent(true)),
			want: []contracts.EventName{lcDelete, lcMarker},
		},
		"suspended sole null marker": {
			setup: func(f *eventFixture) {
				f.put("k", "legacy")
				f.state(contracts.VersioningSuspended)
				f.del(oref("bucket", "k"))
				f.requireIDs(contracts.VersionNull)
				if !f.versions("k")[0].Marker {
					f.t.Fatal("the null version is not a marker")
				}
			},
			decider: soleMarker(contracts.LifecyclePlan{}),
			want:    []contracts.EventName{lcDelete},
		},
		"suspended legacy key expiry": {
			setup: func(f *eventFixture) {
				f.put("k", "legacy")
				f.state(contracts.VersioningSuspended)
			},
			decider: expireOnly, want: []contracts.EventName{lcDelete, lcMarker},
		},
	}
}

// forEachLifecycleCase runs run for each lifecycleEventCase, on an
// eventFixture its setup left.
func forEachLifecycleCase(
	t *testing.T,
	run func(t *testing.T, f *eventFixture, tc lifecycleEventCase),
) {
	cases := lifecycleEventCases()
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range cases {
			if !yield(n) {
				return
			}
		}
	}) {
		t.Run(name, func(t *testing.T) {
			f := newEventFixture(t)
			cases[name].setup(f)
			run(t, f, cases[name])
		})
	}
}

// wantLifecycleEvents returns the events out, the outcome of tc's
// ApplyLifecycle, records: a Delete of each version or marker it deleted,
// in order, then the DeleteMarkerCreated of its marker. It fails unless
// their names are tc's.
func wantLifecycleEvents(
	t *testing.T,
	tc lifecycleEventCase,
	out *contracts.LifecycleOutcome,
	unversioned bool,
) []contracts.BucketEvent {
	t.Helper()
	var want []contracts.BucketEvent
	for _, d := range out.Deleted {
		want = append(want, contracts.BucketEvent{
			Name: lcDelete, Bucket: "bucket", Key: "k", VersionID: d.VersionID,
		})
	}
	if out.MarkerID != "" {
		want = append(want, contracts.BucketEvent{
			Name: lcMarker, Bucket: "bucket", Key: "k", VersionID: out.MarkerID,
		})
	}
	for _, ev := range want {
		if (ev.VersionID == "") != unversioned {
			t.Fatalf("event %+v, unversioned %v", ev, unversioned)
		}
	}
	names := make([]contracts.EventName, len(want))
	for i, ev := range want {
		names[i] = ev.Name
	}
	if !slices.Equal(names, tc.want) {
		t.Fatalf("outcome %+v gives events %v, want %v", out, names, tc.want)
	}
	return want
}

// requireLifecycleEvents fails unless got is want, but for times and
// sources, with strictly increasing sequencers, above floor, each from
// lifecycle's own source with its own request ID.
func requireLifecycleEvents(t *testing.T, got, want []contracts.BucketEvent, floor int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events %+v, want %+v", got, want)
	}
	ids := map[string]bool{}
	for i := range got {
		requireEvent(t, &got[i], &want[i])
		if got[i].Sequence <= floor {
			t.Fatalf("sequence %d after %d", got[i].Sequence, floor)
		}
		floor = got[i].Sequence
		requireLifecycleSource(t, got[i].Source)
		if ids[got[i].Source.RequestID] {
			t.Fatalf("request ID %q repeated", got[i].Source.RequestID)
		}
		ids[got[i].Source.RequestID] = true
	}
}

// requestIDRe matches a request ID as httpx.RequestID makes them.
var requestIDRe = regexp.MustCompile(`^[0-9A-F]{16}$`)

// requireLifecycleSource fails unless src is the source of a lifecycle
// event: S3's own principal and address, and an ID pair in the formats of
// x-amz-request-id and x-amz-id-2.
func requireLifecycleSource(t *testing.T, src contracts.EventSource) {
	t.Helper()
	host, err := base64.StdEncoding.DecodeString(src.HostID)
	if src.PrincipalID != "s3.amazonaws.com" || src.SourceIP != "s3.amazonaws.com" ||
		!requestIDRe.MatchString(src.RequestID) || err != nil || len(host) != 48 {
		t.Fatalf("source %+v (%v)", src, err)
	}
}

// unversioned reports whether "bucket" is Unversioned.
func (f *eventFixture) unversioned() bool {
	f.t.Helper()
	st, err := f.s.versioning("bucket")
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Status == contracts.Unversioned
}

// maxSeq returns the greatest sequence of the versions of "k".
func (f *eventFixture) maxSeq() int64 {
	f.t.Helper()
	var floor int64
	for _, v := range f.versions("k") {
		floor = max(floor, v.Seq)
	}
	if e, err := f.s.legacyEntry(f.ctx, "bucket", "k"); err == nil && e != nil {
		floor = max(floor, e.Seq)
	}
	return floor
}

// Each lifecycle action records the event S3 sends for it, in the order of
// the actions, from lifecycle's own source whatever the context carries,
// and a later change to the key sorts after them.
func TestLifecycleEventRecordingMatrix(t *testing.T) {
	forEachLifecycleCase(t, func(t *testing.T, f *eventFixture, tc lifecycleEventCase) {
		unversioned := f.unversioned()
		floor := f.maxSeq()
		f.configure()
		f.ctx = contracts.WithEventSource(f.ctx, contracts.EventSource{
			PrincipalID: "AIDA1", SourceIP: "127.0.0.1", RequestID: "r", HostID: "h",
		})
		out := f.apply(tc.decider())
		want := wantLifecycleEvents(t, tc, out, unversioned)
		got := f.pending()
		requireLifecycleEvents(t, got, want, floor)
		if int(f.signals.Load()) != len(got) || len(f.outbox()) != 0 {
			t.Fatalf("signaled %d times, outbox left %v", f.signals.Load(), f.outbox())
		}
		if len(got) == 0 {
			return
		}
		f.put("k", "later")
		later := f.pending()
		if len(later) != 1 || later[0].Sequence <= got[len(got)-1].Sequence {
			t.Fatalf("later events %+v after %+v", later, got)
		}
	})
}

// Without a notification configuration, lifecycle records nothing.
func TestLifecycleWithoutConfigurationRecordsNothing(t *testing.T) {
	forEachLifecycleCase(t, func(t *testing.T, f *eventFixture, tc lifecycleEventCase) {
		f.apply(tc.decider())
		if got := f.outbox(); got != nil || f.signals.Load() != 0 {
			t.Fatalf("outbox %v, signaled %d times", got, f.signals.Load())
		}
	})
}

// An upload lifecycle aborts records nothing: S3 has no event for it.
func TestAbortLifecycleUploadRecordsNothing(t *testing.T) {
	f := newEventFixture(t)
	f.configure()
	id, _ := f.upload("k", "part")
	upload, err := f.s.loadUpload(f.ctx, "bucket", "k", id)
	if err != nil {
		t.Fatal(err)
	}
	if aborted, aErr := f.abortLifecycle("k", id, upload.Initiated); aErr != nil || !aborted {
		t.Fatalf("aborted %v, %v", aborted, aErr)
	}
	if got := f.outbox(); got != nil || f.signals.Load() != 0 {
		t.Fatalf("outbox %v, signaled %d times", got, f.signals.Load())
	}
}

// crashAtNth makes f's service fail once, at the nth time it reaches
// stage, as a crash there would stop it.
func crashAtNth(f *fixture, stage string, n int) {
	f.s.hook = func(at string) error {
		if at != stage {
			return nil
		}
		if n--; n > 0 {
			return nil
		}
		f.s.hook = nil
		return errCrash
	}
}

// lifecycleCrash is a stage, the nth time it is reached, that a lifecycle
// change of n events stops at, and what RecoverEvents does after.
type lifecycleCrash struct {
	stage     string
	nth       int
	want      contracts.RecoveredEvents
	committed bool
}

// lifecycleCrashes are the stops of a change of n events: at each intent,
// before the commit, which drops the intents written; once committed,
// which confirms them all; and at each confirm, which confirms those not
// confirmed yet.
func lifecycleCrashes(n int) []lifecycleCrash {
	var out []lifecycleCrash
	for k := 1; k <= n; k++ {
		out = append(out, lifecycleCrash{
			stage: stageEventIntent, nth: k, want: contracts.RecoveredEvents{Dropped: k},
		})
	}
	out = append(out, lifecycleCrash{
		stage: stageCommitted, nth: 1, want: contracts.RecoveredEvents{Confirmed: n},
		committed: true,
	})
	for k := 1; k <= n; k++ {
		out = append(out, lifecycleCrash{
			stage: stageEventConfirmed, nth: k,
			want: contracts.RecoveredEvents{Confirmed: n - k + 1}, committed: true,
		})
	}
	return out
}

// A lifecycle change that stops at any stage of recording any of its
// events leaves what RecoverEvents settles as the stop requires: before
// the commit, no event; after it, every event, in order, none twice.
func TestRecoverLifecycleEventsAfterACrash(t *testing.T) {
	for name, tc := range lifecycleEventCases() {
		if len(tc.want) == 0 {
			continue
		}
		for _, c := range lifecycleCrashes(len(tc.want)) {
			t.Run(name+"/"+c.stage+"/"+strconv.Itoa(c.nth), func(t *testing.T) {
				f := newEventFixture(t)
				tc.setup(f)
				crashLifecycleAndRecover(t, f, tc, c)
			})
		}
	}
}

// crashLifecycleAndRecover stops tc's change at c, opens the store again,
// and checks what RecoverEvents does, and that ApplyLifecycle, run again,
// finishes the change without recording its events twice.
func crashLifecycleAndRecover(
	t *testing.T,
	f *eventFixture,
	tc lifecycleEventCase,
	c lifecycleCrash,
) {
	unversioned := f.unversioned()
	floor := f.maxSeq()
	before := f.ids("k")
	f.configure()
	crashAtNth(f.fixture, c.stage, c.nth)
	_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", tc.decider())
	requireIs(t, "the stopped change", err, errCrash)
	f.restart()
	out, err := f.s.RecoverEvents(f.ctx)
	if err != nil || out != c.want {
		t.Fatalf("recovered %+v, %v; want %+v", out, err, c.want)
	}
	got := f.pending()
	if len(f.outbox()) != 0 {
		t.Fatalf("outbox left %v", f.outbox())
	}
	if !c.committed {
		if len(got) != 0 || !slices.Equal(f.ids("k"), before) {
			t.Fatalf("events %+v, versions %v, want none and %v", got, f.ids("k"), before)
		}
		// Run again, the change records its events once.
		outcome := f.apply(tc.decider())
		requireLifecycleEvents(t, f.pending(), wantLifecycleEvents(t, tc, outcome, unversioned),
			floor)
		return
	}
	requireRecoveredLifecycleEvents(t, got, tc.want)
}

// requireRecoveredLifecycleEvents fails unless got, the events recovery
// confirmed, are want's, in order, from lifecycle's source.
func requireRecoveredLifecycleEvents(
	t *testing.T,
	got []contracts.BucketEvent,
	want []contracts.EventName,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events %+v, want %v", got, want)
	}
	for i := range got {
		if got[i].Name != want[i] || got[i].Key != "k" ||
			(i > 0 && got[i].Sequence <= got[i-1].Sequence) {
			t.Fatalf("events %+v, want %v", got, want)
		}
		requireLifecycleSource(t, got[i].Source)
	}
}

// A lifecycle change whose confirms fail commits, and marks each of its
// events committed, which ConfirmCommitted confirms, in order and before
// the event of a later change to the key that falsifies their witnesses.
func TestLifecycleEventsMarkedCommitted(t *testing.T) {
	forEachLifecycleCase(t, func(t *testing.T, f *eventFixture, tc lifecycleEventCase) {
		if len(tc.want) == 0 {
			return
		}
		f.configure()
		f.failRender(func() { f.apply(tc.decider()) })
		marks := 0
		for _, n := range f.outbox() {
			if strings.HasSuffix(n, committedExt) {
				marks++
			}
		}
		if marks != len(tc.want) || len(f.outbox()) != marks {
			t.Fatalf("outbox %v, want %d marks", f.outbox(), len(tc.want))
		}
		f.put("k", "later")
		f.restart()
		if out, err := f.s.RecoverEvents(f.ctx); err != nil ||
			out != (contracts.RecoveredEvents{}) {
			t.Fatalf("recovered %+v, %v", out, err)
		}
		f.confirmCommitted(len(tc.want))
		got := f.pending()
		names := make([]contracts.EventName, len(got))
		for i, ev := range got {
			names[i] = ev.Name
		}
		want := append(slices.Clone(tc.want), contracts.EventObjectCreatedPut)
		if !slices.Equal(names, want) {
			t.Fatalf("events %v, want %v", names, want)
		}
	})
}

// A lifecycle change whose manifest commit fails is settled at once: its
// intents are removed, and no event is recorded.
func TestAFailedLifecycleCommitRecordsNothing(t *testing.T) {
	for _, name := range []string{"enabled noncurrent and expiry", "suspended noncurrent null expiry"} {
		t.Run(name, func(t *testing.T) {
			tc := lifecycleEventCases()[name]
			f := newEventFixture(t)
			tc.setup(f)
			before := f.ids("k")
			f.configure()
			restore := failManifestWrites(f.fixture)
			_, err := f.s.ApplyLifecycle(f.ctx, "bucket", "k", tc.decider())
			restore()
			if err == nil {
				t.Fatal("the change committed")
			}
			if len(f.outbox()) != 0 || !slices.Equal(f.ids("k"), before) {
				t.Fatalf("outbox %v, versions %v; want none and %v", f.outbox(), f.ids("k"),
					before)
			}
		})
	}
}

// With the notification package's Router, lifecycle's events match
// s3:LifecycleExpiration:* and render lifecycle's principal and address;
// s3:ObjectRemoved:* matches none of them (S3 User Guide, "Event
// notification types and destinations").
func TestLifecycleEventsWithTheNotificationRouter(t *testing.T) {
	for event, want := range map[string]int{
		"s3:LifecycleExpiration:*": 3, "s3:ObjectRemoved:*": 0,
	} {
		t.Run(event, func(t *testing.T) {
			cfg, err := notification.Parse([]byte(`<NotificationConfiguration>` +
				`<QueueConfiguration><Id>c</Id>` +
				`<Queue>arn:aws:sqs:us-east-1:000000000000:q</Queue>` +
				`<Event>` + event + `</Event></QueueConfiguration></NotificationConfiguration>`))
			if err != nil {
				t.Fatal(err)
			}
			router := notification.NewRouter("000000000000",
				map[contracts.DestinationType]bool{contracts.DestinationQueue: true})
			s := openEventService(t, t.TempDir(), Events{Router: router})
			if _, err = s.CreateBucket(context.Background(), "bucket", nil); err != nil {
				t.Fatal(err)
			}
			f := &eventFixture{fixture: pinnedFixture(t, s)}
			lifecycleEventCases()["enabled noncurrent and expiry"].setup(f)
			doc, err := notification.Encode(ref(t, s, "bucket").Generation, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.PutBucketConfig(f.ctx, "bucket", contracts.ConfigNotification,
				doc); err != nil {
				t.Fatal(err)
			}
			f.apply(planFor(removeNoncurrent(true))())
			evs, err := s.PendingEvents(f.ctx, 10, contracts.PendingQuery{})
			if err != nil || len(evs) != want {
				t.Fatalf("events %d (%v), want %d", len(evs), err, want)
			}
			for i, pe := range evs {
				name := lcDelete
				if i == 2 {
					name = lcMarker
				}
				requireLifecycleBody(t, pe.Body, name)
			}
		})
	}
}

// requireLifecycleBody fails unless body, a rendered message, is of an
// event name from lifecycle's principal and address.
func requireLifecycleBody(t *testing.T, body []byte, name contracts.EventName) {
	t.Helper()
	for _, part := range []string{
		`"eventName":"` + string(name) + `"`, `"principalId":"s3.amazonaws.com"`,
		`"sourceIPAddress":"s3.amazonaws.com"`,
	} {
		if !bytes.Contains(body, []byte(part)) {
			t.Fatalf("body %s lacks %s", body, part)
		}
	}
}

// A failed sync of the outbox's directory, after each intent and event is
// renamed into place, as of every other file, does not stop a lifecycle
// change: it commits, and records every event.
func TestLifecycleEventsSurviveFailedDirectorySyncs(t *testing.T) {
	forEachLifecycleCase(t, func(t *testing.T, f *eventFixture, tc lifecycleEventCase) {
		f.configure()
		errSync := errors.New("sync")
		f.s.hook = func(stage string) error {
			if stage == stageDirSync {
				return errSync
			}
			return nil
		}
		out := f.apply(tc.decider())
		f.s.hook = nil
		want := wantLifecycleEvents(t, tc, out, f.unversioned())
		got := f.pending()
		if len(got) != len(want) || len(f.outbox()) != 0 {
			t.Fatalf("events %+v, outbox %v; want %d events", got, f.outbox(), len(want))
		}
		for i := range got {
			requireEvent(t, &got[i], &want[i])
		}
	})
}

// A change whose marker is not in the manifest, which removeSoleMarker
// keeps from happening, is refused before any intent is written: its
// events would have no witness to recover them by.
func TestALifecycleMarkerMissingFromTheManifestIsRefused(t *testing.T) {
	f := newEventFixture(t)
	f.state(contracts.VersioningEnabled)
	v1 := f.put("k", "v1")
	f.put("k", "v2")
	f.configure()
	before, _, err := f.s.readVersions("bucket", "k")
	if err != nil {
		t.Fatal(err)
	}
	unlock := f.s.locks.lock("bucket", "k")
	m, _, err := f.s.loadVersions(f.ctx, "bucket", "k")
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	e := m.remove(m.find(v1.VersionID))
	a := &lifecycleChange{out: &contracts.LifecycleOutcome{
		MarkerID: newVersionID(f.s.nextSeq(0)), MarkerRule: "cur",
	}}
	a.remove(e, "nc")
	err = f.s.commitLifecycle(f.ctx, "bucket", m, a)
	unlock()
	if err == nil || !strings.Contains(err.Error(), "not in the manifest") {
		t.Fatalf("committed a change without its marker: %v", err)
	}
	if got := f.outbox(); got != nil {
		t.Fatalf("outbox %v", got)
	}
	after, _, err := f.s.readVersions("bucket", "k")
	if err != nil || len(after.Versions) != len(before.Versions) {
		t.Fatalf("versions %+v (%v), want %+v", after, err, before)
	}
	if f.signals.Load() != 0 {
		t.Fatal("signaled")
	}
}
