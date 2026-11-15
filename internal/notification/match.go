package notification

import (
	"net/url"
	"strings"

	"tule.dev/object-store/internal/contracts"
)

// EncodeKey returns key as an event message reports it and as filters
// match it: each "/"-separated segment form-encoded (url.QueryEscape), so
// that a space is "+" and other special characters are percent-encoded,
// joined with a literal "/" (N5). "red flower.jpg" becomes
// "red+flower.jpg" (S3 User Guide, "Event message structure"); the guide
// shows no other character. Everything beyond the space is a choice,
// unverified against a live account: url.QueryEscape writes "*" as "%2A"
// and keeps "~", where a Java-style form encoder, which S3 may use, keeps
// "*" and writes "~" as "%7E".
func EncodeKey(key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.QueryEscape(s)
	}
	return strings.Join(segments, "/")
}

// Match returns the destination an event named name on key is sent to:
// the first configuration, topics then queues then functions, one of
// whose event types matches name and whose filter admits the key. V9
// leaves at most one. Filters match the key as EncodeKey encodes it,
// since their values must be given "+" for a space and percent-encoded
// (N6).
func (c *Configuration) Match(name contracts.EventName, key string) (contracts.Destination, bool) {
	encoded := EncodeKey(key)
	for _, k := range kinds {
		for _, cfg := range c.configs(k) {
			if cfg.matches(name, encoded) {
				return contracts.Destination{
					Type:            k.typ,
					ARN:             cfg.ARN,
					ConfigurationID: cfg.ID,
				}, true
			}
		}
	}
	return contracts.Destination{}, false
}

// matches reports whether c sends an event named name on the encoded key.
func (c *Config) matches(name contracts.EventName, encodedKey string) bool {
	prefix, suffix := c.Filter.affixes()
	if !strings.HasPrefix(encodedKey, prefix) || !strings.HasSuffix(encodedKey, suffix) {
		return false
	}
	for _, ev := range c.Events {
		for _, n := range eventTypes[ev] {
			if n == name {
				return true
			}
		}
	}
	return false
}
