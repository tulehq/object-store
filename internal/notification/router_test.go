package notification

import (
	"fmt"
	"sync"
	"testing"

	"tule.dev/object-store/internal/contracts"
)

func storedDoc(t *testing.T, generation, body string) []byte {
	t.Helper()
	c, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(generation, c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var allAvailable = map[contracts.DestinationType]bool{
	contracts.DestinationQueue: true, contracts.DestinationFunction: true,
	contracts.DestinationTopic: true,
}

func routerDoc(t *testing.T) []byte {
	t.Helper()
	return storedDoc(t, "g1", doc(
		"<TopicConfiguration><Id>t</Id>"+ev("s3:ObjectTagging:*")+
			"<Topic>"+topicARN+"</Topic></TopicConfiguration>",
		queue("<Id>q</Id>"+ev(put)+filter(filterRule("prefix", "in/"))),
		"<CloudFunctionConfiguration><Id>f</Id>"+ev("s3:ObjectRemoved:*")+
			"<CloudFunction>"+functionARN+"</CloudFunction></CloudFunctionConfiguration>",
	))
}

func TestRoute(t *testing.T) {
	raw := routerDoc(t)
	r := NewRouter(account, allAvailable)
	put := &contracts.BucketEvent{Name: contracts.EventObjectCreatedPut, Key: "in/a b"}
	d, ok, err := r.Route(raw, "g1", put)
	want := contracts.Destination{
		Type: contracts.DestinationQueue, ARN: queueARN,
		ConfigurationID: "q",
	}
	if err != nil || !ok || d != want {
		t.Errorf("put: %+v %v %v", d, ok, err)
	}
	for name, ev := range map[string]*contracts.BucketEvent{
		"unmatched key":   {Name: contracts.EventObjectCreatedPut, Key: "out/a"},
		"unmatched event": {Name: contracts.EventObjectCreatedCopy, Key: "in/a"},
		"a topic":         {Name: contracts.EventObjectTaggingPut, Key: "in/a"},
	} {
		if d, ok, err := r.Route(raw, "g1", ev); err != nil || ok {
			t.Errorf("%s: %+v %v %v", name, d, ok, err)
		}
	}
	if d, ok, err := r.Route(raw, "g2", put); err != nil || ok {
		t.Errorf("another generation: %+v %v %v", d, ok, err)
	}
}

func TestRouteSkipsUnavailableTypes(t *testing.T) {
	raw := routerDoc(t)
	r := NewRouter(account, map[contracts.DestinationType]bool{contracts.DestinationQueue: true})
	del := &contracts.BucketEvent{Name: contracts.EventObjectRemovedDelete, Key: "k"}
	if _, ok, err := r.Route(raw, "g1", del); err != nil || ok {
		t.Errorf("function without endpoint: %v %v", ok, err)
	}
	put := &contracts.BucketEvent{Name: contracts.EventObjectCreatedPut, Key: "in/k"}
	if _, ok, err := r.Route(raw, "g1", put); err != nil || !ok {
		t.Errorf("queue: %v %v", ok, err)
	}
}

func TestRouteRefusesACorruptFile(t *testing.T) {
	r := NewRouter(account, allAvailable)
	if _, _, err := r.Route([]byte("{"), "g1", &contracts.BucketEvent{}); err == nil {
		t.Error("routed a corrupt file")
	}
}

func TestRouteCacheIsBoundedAndConcurrent(t *testing.T) {
	r := NewRouter(account, allAvailable)
	docs := make([][]byte, cacheSize+10)
	for i := range docs {
		docs[i] = storedDoc(t, fmt.Sprint("g", i), doc(queue(ev(put))))
	}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i, raw := range docs {
				_, ok, err := r.Route(raw, fmt.Sprint("g", i),
					&contracts.BucketEvent{Name: contracts.EventObjectCreatedPut, Key: "k"})
				if err != nil || !ok {
					t.Errorf("worker %d doc %d: %v %v", w, i, ok, err)
				}
			}
		})
	}
	wg.Wait()
	if len(r.cache) != cacheSize || len(r.order) != cacheSize {
		t.Errorf("cache %d, order %d", len(r.cache), len(r.order))
	}
}
