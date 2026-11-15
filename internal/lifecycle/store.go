package lifecycle

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
)

// Stored is a bucket's stored lifecycle configuration: the bucket
// generation it was put in, the x-amz-transition-default-minimum-object-
// size it was put with, and Document, the configuration's canonical
// encoding, which is exactly what GetBucketLifecycleConfiguration
// answers. Keeping the three in one file commits them in one write.
type Stored struct {
	Generation                         string `json:"generation"`
	TransitionDefaultMinimumObjectSize string `json:"transitionDefaultMinimumObjectSize"`
	Document                           string `json:"document"`
}

// Encode returns the stored form of c, a configuration Parse returned,
// put in the bucket generation with the header value transitionDefault.
// The document is c with the XML declaration and the S3 namespace; each
// rule keeps the form it was given in, a top-level Prefix or a Filter, an
// empty Filter as an empty Filter.
func Encode(generation string, c *Configuration, transitionDefault string) ([]byte, error) {
	doc := *c
	doc.Xmlns = xmlns
	b, err := xml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: encode configuration: %w", err)
	}
	raw, err := json.Marshal(Stored{
		Generation:                         generation,
		TransitionDefaultMinimumObjectSize: transitionDefault,
		Document:                           xml.Header + string(b),
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: encode stored configuration: %w", err)
	}
	return raw, nil
}

// Decode reads a stored configuration Encode wrote. The document is not
// validated again: it was when it was put.
func Decode(raw []byte) (*Stored, *Configuration, error) {
	var s Stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, fmt.Errorf("lifecycle: decode stored configuration: %w", err)
	}
	var c Configuration
	if err := xml.Unmarshal([]byte(s.Document), &c); err != nil {
		return nil, nil, fmt.Errorf("lifecycle: decode stored document: %w", err)
	}
	return &s, &c, nil
}
