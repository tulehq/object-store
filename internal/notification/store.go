package notification

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
)

// Stored is a bucket's stored notification configuration: the bucket
// generation it was put in and Document, the configuration's canonical
// encoding, which is exactly what GetBucketNotificationConfiguration
// answers. Keeping the two in one file commits them in one write.
type Stored struct {
	Generation string `json:"generation"`
	Document   string `json:"document"`
}

// Encode returns the stored form of c, a configuration Parse returned, put
// in the bucket generation. The document is c with the XML declaration and
// the S3 namespace, its elements in the order Configuration names.
func Encode(generation string, c *Configuration) ([]byte, error) {
	doc := *c
	doc.Xmlns = xmlns
	b, err := xml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("notification: encode configuration: %w", err)
	}
	raw, err := json.Marshal(Stored{Generation: generation, Document: xml.Header + string(b)})
	if err != nil {
		return nil, fmt.Errorf("notification: encode stored configuration: %w", err)
	}
	return raw, nil
}

// Decode reads a stored configuration Encode wrote. The document is not
// validated again: it was when it was put.
func Decode(raw []byte) (*Stored, *Configuration, error) {
	var s Stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, fmt.Errorf("notification: decode stored configuration: %w", err)
	}
	var c Configuration
	if err := xml.Unmarshal([]byte(s.Document), &c); err != nil {
		return nil, nil, fmt.Errorf("notification: decode stored document: %w", err)
	}
	return &s, &c, nil
}

// The element twins of Configuration and Config, one Config twin per
// kind, which names its ARN element. A Config converts to each, as their
// fields differ only in their tags. The field order is the element order.
//
//nolint:govet // fieldalignment: as above
type (
	xmlConfiguration struct {
		XMLName     xml.Name      `xml:"NotificationConfiguration"`
		Xmlns       string        `xml:"xmlns,attr,omitempty"`
		Topics      []xmlTopic    `xml:"TopicConfiguration"`
		Queues      []xmlQueue    `xml:"QueueConfiguration"`
		Functions   []xmlFunction `xml:"CloudFunctionConfiguration"`
		EventBridge *struct{}     `xml:"EventBridgeConfiguration"`
	}

	xmlTopic struct {
		ID     string   `xml:"Id"`
		ARN    string   `xml:"Topic"`
		Events []string `xml:"Event"`
		Filter *Filter  `xml:"Filter"`
	}

	xmlQueue struct {
		ID     string   `xml:"Id"`
		ARN    string   `xml:"Queue"`
		Events []string `xml:"Event"`
		Filter *Filter  `xml:"Filter"`
	}

	xmlFunction struct {
		ID     string   `xml:"Id"`
		ARN    string   `xml:"CloudFunction"`
		Events []string `xml:"Event"`
		Filter *Filter  `xml:"Filter"`
	}
)

// MarshalXML implements xml.Marshaler.
func (c *Configuration) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	x := xmlConfiguration{Xmlns: c.Xmlns, EventBridge: c.EventBridge}
	for i := range c.Topics {
		x.Topics = append(x.Topics, xmlTopic(c.Topics[i]))
	}
	for i := range c.Queues {
		x.Queues = append(x.Queues, xmlQueue(c.Queues[i]))
	}
	for i := range c.Functions {
		x.Functions = append(x.Functions, xmlFunction(c.Functions[i]))
	}
	start.Name = xml.Name{Local: "NotificationConfiguration"}
	return enc.EncodeElement(&x, start) //nolint:wrapcheck // encoding/xml's, as Marshal returns it
}

// UnmarshalXML implements xml.Unmarshaler, for the canonical document only:
// a request document is read by Parse.
func (c *Configuration) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	var x xmlConfiguration
	if err := dec.DecodeElement(&x, &start); err != nil {
		return err //nolint:wrapcheck // encoding/xml's, as Unmarshal returns it
	}
	*c = Configuration{XMLName: x.XMLName, Xmlns: x.Xmlns, EventBridge: x.EventBridge}
	for i := range x.Topics {
		c.Topics = append(c.Topics, Config(x.Topics[i]))
	}
	for i := range x.Queues {
		c.Queues = append(c.Queues, Config(x.Queues[i]))
	}
	for i := range x.Functions {
		c.Functions = append(c.Functions, Config(x.Functions[i]))
	}
	return nil
}
