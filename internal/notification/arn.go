package notification

import (
	"fmt"
	"strings"

	"tule.dev/object-store/internal/contracts"
	"tule.dev/object-store/internal/s3api"
)

// ARN is a destination's ARN, arn:Partition:Service:Region:Account:Resource.
// Resource is a queue's or topic's name, or function:<name>[:<qualifier>]
// for a function.
type ARN struct {
	Partition string
	Service   string
	Region    string
	Account   string
	Resource  string
}

// The name limits: a queue name is "up to 80 characters ... alphanumeric
// characters, hyphens (-), and underscores (_)" with ".fifo" for a FIFO
// queue (SQS CreateQueue), a topic name "up to 256 characters" of the
// same (SNS CreateTopic), and a function name 1 to 64 characters of the
// same (Lambda FunctionConfiguration); a qualifier is a version, an alias
// of the same characters, or $LATEST.
const (
	maxQueueName     = 80
	maxTopicName     = 256
	maxFunctionName  = 64
	maxQualifier     = 128
	accountIDLength  = 12
	functionResource = "function:"
	fifoSuffix       = ".fifo"
	latestQualifier  = "$LATEST"
)

// ParseDestinationARN parses the ARN of a destination of type t. It
// refuses (V7), as a 400 InvalidArgument whose argument names t's element
// and the ARN, one that is not an ARN, is of another partition than aws or
// another service than t's, or names a region, account or resource that
// is not well formed. The region is not compared with the bucket's.
func ParseDestinationARN(t contracts.DestinationType, arn string) (ARN, error) {
	k, ok := kindOf(t)
	if !ok {
		return ARN{}, fmt.Errorf("notification: unknown destination type %q", t)
	}
	refuse := argumentError(msgARN, []s3api.ErrorArgument{{Name: k.arn, Value: arn}})
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return ARN{}, refuse
	}
	a := ARN{
		Partition: parts[1], Service: parts[2], Region: parts[3], Account: parts[4],
		Resource: parts[5],
	}
	if a.Partition != "aws" || a.Service != k.service || !isRegion(a.Region) ||
		!isAccountID(a.Account) || !validResource(t, a.Resource) {
		return ARN{}, refuse
	}
	return a, nil
}

// validResource reports whether r is a well-formed resource of type t.
func validResource(t contracts.DestinationType, r string) bool {
	switch t {
	case contracts.DestinationQueue:
		return isQueueOrTopicName(r, maxQueueName)
	case contracts.DestinationTopic:
		return isQueueOrTopicName(r, maxTopicName)
	case contracts.DestinationFunction:
		rest, ok := strings.CutPrefix(r, functionResource)
		if !ok {
			return false
		}
		name, qualifier, qualified := strings.Cut(rest, ":")
		if !isName(name, maxFunctionName) {
			return false
		}
		return !qualified || qualifier == latestQualifier || isName(qualifier, maxQualifier)
	}
	return false
}

// isQueueOrTopicName reports whether r is a queue or topic name of at most
// n characters, a FIFO one's ".fifo" counted in them.
func isQueueOrTopicName(r string, n int) bool {
	if base, fifo := strings.CutSuffix(r, fifoSuffix); fifo {
		return isName(base, n-len(fifoSuffix))
	}
	return isName(r, n)
}

// isName reports whether s is 1 to n letters, digits, hyphens and
// underscores.
func isName(s string, n int) bool {
	if s == "" || len(s) > n {
		return false
	}
	for _, c := range s {
		if !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// isRegion reports whether s has the form of a region name: lower-case
// letters, digits and hyphens, starting with a letter.
func isRegion(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// isAccountID reports whether s is a 12-digit account ID.
func isAccountID(s string) bool {
	if len(s) != accountIDLength {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isAlnum(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
