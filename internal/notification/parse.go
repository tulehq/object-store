package notification

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strings"

	"tule.dev/object-store/internal/s3api"
)

// The wire twins of the document types. Every element is read as a slice,
// so that decode can refuse, with MalformedXML, an element repeated where
// the schema allows one, which encoding/xml would otherwise read as the
// last value given. A configuration reads all three ARN elements and uses
// the one its kind names; the others are unknown elements, and ignored.
type (
	wireConfig struct {
		IDs            []string     `xml:"Id"`
		Topics         []string     `xml:"Topic"`
		Queues         []string     `xml:"Queue"`
		CloudFunctions []string     `xml:"CloudFunction"`
		Events         []string     `xml:"Event"`
		Filters        []wireFilter `xml:"Filter"`
	}

	wireFilter struct {
		S3Keys []wireS3Key `xml:"S3Key"`
	}

	wireS3Key struct {
		Rules []wireRule `xml:"FilterRule"`
	}

	wireRule struct {
		Names  []string `xml:"Name"`
		Values []string `xml:"Value"`
	}
)

// entry is one configuration of the document, with its kind, in document
// order.
type entry struct {
	kind kind
	wire wireConfig
}

// wireDocument is the request document: its root, its configurations in
// document order, and how many EventBridgeConfiguration elements it has.
type wireDocument struct {
	root        string
	entries     []entry
	eventBridge int
}

// UnmarshalXML implements xml.Unmarshaler, keeping the document order of
// the three configuration elements, which the validation follows.
func (w *wireDocument) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	w.root = start.Name.Local
	for {
		tok, err := dec.Token()
		if err != nil {
			return err //nolint:wrapcheck // the decode error Parse answers as MalformedXML
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil
		case xml.StartElement:
			if err := w.child(dec, &t); err != nil {
				return err
			}
		}
	}
}

func (w *wireDocument) child(dec *xml.Decoder, t *xml.StartElement) error {
	if t.Name.Local == "EventBridgeConfiguration" {
		w.eventBridge++
		return dec.Skip() //nolint:wrapcheck // as above
	}
	for _, k := range kinds {
		if t.Name.Local == k.element {
			e := entry{kind: k}
			if err := dec.DecodeElement(&e.wire, t); err != nil {
				return err //nolint:wrapcheck // as above
			}
			w.entries = append(w.entries, e)
			return nil
		}
	}
	return dec.Skip() //nolint:wrapcheck // as above
}

// Parse decodes and validates a PutBucketNotificationConfiguration body:
// first the shape of the whole document (V1), then configuration by
// configuration in document order (V2 to V8), then across configurations
// (V6, V9). It answers the first error found as the S3 error the request
// fails with (see validate.go). A configuration without an Id, or with an
// empty one, is given a generated one. Elements the document model does
// not know are ignored.
func Parse(doc []byte) (*Configuration, error) {
	if len(doc) > MaxDocument {
		return nil, s3api.ErrMaxMessageLengthExceeded()
	}
	var w wireDocument
	if err := xml.NewDecoder(bytes.NewReader(doc)).Decode(&w); err != nil ||
		w.root != "NotificationConfiguration" || w.eventBridge > 1 {
		return nil, s3api.ErrMalformedXML()
	}
	decoded := make([]decodedConfig, 0, len(w.entries))
	for i := range w.entries {
		d, ok := w.entries[i].decode()
		if !ok {
			return nil, s3api.ErrMalformedXML()
		}
		decoded = append(decoded, d)
	}
	if err := checkConfigs(decoded); err != nil {
		return nil, err
	}
	assignIDs(decoded)
	return configuration(decoded, w.eventBridge == 1), nil
}

// configuration returns the configuration of the checked configurations,
// each in the slice of its kind.
func configuration(decoded []decodedConfig, eventBridge bool) *Configuration {
	c := &Configuration{Xmlns: xmlns}
	if eventBridge {
		c.EventBridge = &struct{}{}
	}
	for _, d := range decoded {
		switch d.kind.typ {
		case topicKind.typ:
			c.Topics = append(c.Topics, d.config)
		case queueKind.typ:
			c.Queues = append(c.Queues, d.config)
		case functionKind.typ:
			c.Functions = append(c.Functions, d.config)
		}
	}
	return c
}

// ParseSkip validates an x-amz-skip-destination-validation request header
// value: "true" or "false", in any case, and "" for false (N21).
func ParseSkip(h string) (bool, error) {
	switch strings.ToLower(h) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, s3api.ErrInvalidArgument(msgSkip)
}

// decodedConfig is a configuration with its kind, as decoded and before
// it is validated.
type decodedConfig struct {
	config Config
	kind   kind
}

// decode converts e to a configuration; ok is false for a shape error
// (V1): an element repeated where one is allowed, a missing ARN element,
// no Event, or a FilterRule's Name or Value repeated.
func (e *entry) decode() (decodedConfig, bool) {
	w := &e.wire
	arns := map[string][]string{
		topicKind.arn: w.Topics, queueKind.arn: w.Queues, functionKind.arn: w.CloudFunctions,
	}[e.kind.arn]
	if len(w.IDs) > 1 || len(arns) != 1 || len(w.Events) == 0 || len(w.Filters) > 1 {
		return decodedConfig{}, false
	}
	c := Config{ARN: strings.TrimSpace(arns[0]), Events: make([]string, 0, len(w.Events))}
	if len(w.IDs) == 1 {
		c.ID = w.IDs[0]
	}
	for _, ev := range w.Events {
		c.Events = append(c.Events, strings.TrimSpace(ev))
	}
	if len(w.Filters) == 1 {
		f, ok := w.Filters[0].decode()
		if !ok {
			return decodedConfig{}, false
		}
		c.Filter = f
	}
	return decodedConfig{kind: e.kind, config: c}, true
}

// decode converts w to a Filter, nil for one without rules, which admits
// every key. So an empty Filter, or one with an empty S3Key, is dropped
// and GET answers the configuration without it, a choice: S3's echo of
// one is not documented.
func (w *wireFilter) decode() (*Filter, bool) {
	if len(w.S3Keys) > 1 {
		return nil, false
	}
	if len(w.S3Keys) == 0 || len(w.S3Keys[0].Rules) == 0 {
		return nil, true
	}
	f := &Filter{}
	for _, r := range w.S3Keys[0].Rules {
		if len(r.Names) > 1 || len(r.Values) > 1 {
			return nil, false
		}
		var rule FilterRule
		if len(r.Names) == 1 {
			rule.Name = r.Names[0]
		}
		if len(r.Values) == 1 {
			rule.Value = r.Values[0]
		}
		f.S3Key.Rules = append(f.S3Key.Rules, rule)
	}
	return f, true
}

// assignIDs gives each configuration without an Id a generated one, unique
// among them, in the form of the GetBucketNotificationConfiguration API
// reference's sample Id: the standard base64 of a random UUID's text.
func assignIDs(configs []decodedConfig) {
	used := make(map[string]bool, len(configs))
	for i := range configs {
		used[configs[i].config.ID] = true
	}
	for i := range configs {
		if configs[i].config.ID != "" {
			continue
		}
		id := newID()
		for used[id] {
			id = newID()
		}
		used[id] = true
		configs[i].config.ID = id
	}
}

// newID returns the base64 of a random version 4 UUID's text.
func newID() string {
	var u [16]byte
	_, _ = rand.Read(u[:]) // crypto/rand.Read never returns an error
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	text := fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	return base64.StdEncoding.EncodeToString([]byte(text))
}
