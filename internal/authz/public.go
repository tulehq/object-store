package authz

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// A bucket policy is public or not by the rule of "The meaning of
// 'public'" ("Blocking public access to your Amazon S3 storage",
// https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html):
// "Amazon S3 begins by assuming that the policy is public. ... To be
// considered non-public, a bucket policy must grant access only to fixed
// values (values that don't contain a wildcard or an AWS Identity and
// Access Management Policy Variable)" for a principal or one of the keys
// below. An Allow statement whose Principal is "*" (or names "*" under any
// principal type) is public, unless its Condition fixes one of those keys.
// Deny statements grant nothing and are never public. A principal can only
// be "*" or a fixed value, since tule.dev/policy refuses partial wildcards
// in principals.
//
// This is a port of secret-store's internal/authz/public.go, which applies
// the same rule to Secrets Manager's BlockPublicPolicy, with S3's
// s3:DataAccessPointArn and s3:DataAccessPointAccount added. The two copies
// must change together.

// narrowingKeys are the condition keys S3 lists as making a wildcard
// principal non-public when fixed: the principal's identity (aws:PrincipalArn,
// aws:PrincipalAccount, aws:PrincipalOrgID, aws:userid), the request's
// origin (aws:SourceArn, aws:SourceVpc, aws:SourceVpce, aws:SourceOwner,
// aws:SourceAccount) and the access point it came through
// (s3:DataAccessPointArn, s3:DataAccessPointAccount). aws:SourceIp narrows
// too, when every range is at least a /8 (IPv4) or a /32 (IPv6), which is
// S3's threshold ("values broader than /8 for IPv4 and /32 for IPv6" are
// public).
var narrowingKeys = []string{
	"aws:PrincipalArn", "aws:PrincipalAccount", "aws:PrincipalOrgID", "aws:userid",
	"aws:SourceArn", "aws:SourceVpc", "aws:SourceVpce", "aws:SourceOwner", "aws:SourceAccount",
	"s3:DataAccessPointArn", "s3:DataAccessPointAccount",
}

// fixingOperators are the operators that hold only for the values they list.
// A negated operator, ...IfExists (which holds when the key is absent) and
// ForAllValues: (which holds when it is absent) narrow nothing.
var fixingOperators = []string{
	"StringEquals", "StringEqualsIgnoreCase", "StringLike", "ArnEquals", "ArnLike",
}

// PublicPolicy reports whether doc, a bucket policy that
// policy.ParseResource has accepted, is public as described above. Its JSON
// is read with exact element names and no repeated keys, as the parser has
// already required, so it is the document the parser read. A doc that does
// not decode as one is reported as an error, never as non-public.
func PublicPolicy(doc []byte) (bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return false, fmt.Errorf("authz: decode the bucket policy: %w", err)
	}
	statements, err := statementsOf(top["Statement"])
	if err != nil {
		return false, err
	}
	for i, st := range statements {
		broad, err := broadStatement(st)
		if err != nil {
			return false, fmt.Errorf("authz: statement %s: %w", statementName(st, i), err)
		}
		if broad {
			return true, nil
		}
	}
	return false, nil
}

func statementsOf(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	var many []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one map[string]json.RawMessage
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("authz: decode the policy's statements: %w", err)
	}
	return []map[string]json.RawMessage{one}, nil
}

func statementName(st map[string]json.RawMessage, i int) string {
	var sid string
	if err := json.Unmarshal(st["Sid"], &sid); err == nil && sid != "" {
		return strconv.Quote(sid)
	}
	return strconv.Itoa(i + 1)
}

func broadStatement(st map[string]json.RawMessage) (bool, error) {
	var effect string
	if err := json.Unmarshal(st["Effect"], &effect); err != nil {
		return false, fmt.Errorf("decode Effect: %w", err)
	}
	if effect != "Allow" {
		return false, nil
	}
	raw, ok := st["Principal"]
	if !ok {
		// An Allow needs a Principal (NotPrincipal requires Deny); the
		// parser has refused one without.
		return false, nil
	}
	wildcard, err := wildcardPrincipal(raw)
	if err != nil || !wildcard {
		return false, err
	}
	narrowed, err := narrowedBy(st["Condition"])
	if err != nil {
		return false, err
	}
	return !narrowed, nil
}

// wildcardPrincipal reports whether a Principal element is "*" or names "*"
// under any principal type.
func wildcardPrincipal(raw json.RawMessage) (bool, error) {
	var all string
	if err := json.Unmarshal(raw, &all); err == nil {
		return all == "*", nil
	}
	var byType map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byType); err != nil {
		return false, fmt.Errorf("decode Principal: %w", err)
	}
	for _, v := range byType {
		values, err := stringsOf(v)
		if err != nil {
			return false, fmt.Errorf("decode Principal: %w", err)
		}
		for _, p := range values {
			if p == "*" {
				return true, nil
			}
		}
	}
	return false, nil
}

// narrowedBy reports whether a Condition element fixes a narrowing key.
// Conditions are ANDed, so one such key is enough.
func narrowedBy(raw json.RawMessage) (bool, error) {
	if raw == nil {
		return false, nil
	}
	var byOp map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byOp); err != nil {
		return false, fmt.Errorf("decode Condition: %w", err)
	}
	for op, keys := range byOp {
		base, ok := fixingOperator(op)
		if !ok {
			continue
		}
		for key, v := range keys {
			values, err := stringsOf(v)
			if err != nil {
				return false, fmt.Errorf("decode Condition %s: %w", op, err)
			}
			if fixes(base, key, values) {
				return true, nil
			}
		}
	}
	return false, nil
}

// fixingOperator returns the base operator of op when op holds only for the
// values it lists: a fixing operator or IpAddress, plain or under
// ForAnyValue:.
func fixingOperator(op string) (string, bool) {
	if strings.HasSuffix(op, "IfExists") || strings.HasPrefix(op, "ForAllValues:") {
		return "", false
	}
	base := strings.TrimPrefix(op, "ForAnyValue:")
	if base == "IpAddress" {
		return base, true
	}
	for _, f := range fixingOperators {
		if base == f {
			return base, true
		}
	}
	return "", false
}

// fixes reports whether the operator base, on key with values, narrows a
// wildcard principal. Condition key names match regardless of case, as
// tule.dev/policy matches them.
func fixes(base, key string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	if base == "IpAddress" {
		return strings.EqualFold(key, "aws:SourceIp") && all(values, narrowRange)
	}
	for _, k := range narrowingKeys {
		if !strings.EqualFold(key, k) {
			continue
		}
		if k == "aws:userid" && base == "StringLike" {
			return all(values, fixedUserID)
		}
		if k == "s3:DataAccessPointArn" && strings.HasSuffix(base, "Like") {
			return all(values, fixedAccessPoint)
		}
		return all(values, fixed)
	}
	return false
}

func all(values []string, ok func(string) bool) bool {
	for _, v := range values {
		if !ok(v) {
			return false
		}
	}
	return true
}

// fixedUserID reports whether v, a StringLike value of aws:userid, is fixed
// or "AROLEID:*", every session of one role, which S3 counts as fixed
// ("aws:userid, outside the pattern AROLEID:*").
func fixedUserID(v string) bool {
	if fixed(v) {
		return true
	}
	prefix, ok := strings.CutSuffix(v, ":*")
	return ok && strings.HasPrefix(prefix, "AROA") && fixed(prefix)
}

// fixedAccessPoint reports whether v, a StringLike or ArnLike value of
// s3:DataAccessPointArn, is fixed but for the access point's name: "this
// value can contain a wildcard for the access point name without rendering
// the policy public, as long as the account ID is fixed".
func fixedAccessPoint(v string) bool {
	prefix, name, ok := strings.Cut(v, ":accesspoint/")
	return ok && fixed(prefix) && name != "" && !strings.Contains(name, "${")
}

// fixed reports whether a condition value holds no wildcard and no policy
// variable.
func fixed(v string) bool {
	return v != "" && !strings.ContainsAny(v, "*?") && !strings.Contains(v, "${")
}

// narrowRange reports whether v, an aws:SourceIp value, is an address or a
// range no broader than a /8 for IPv4 or a /32 for IPv6.
func narrowRange(v string) bool {
	if addr, err := netip.ParseAddr(v); err == nil {
		return addr.IsValid()
	}
	p, err := netip.ParsePrefix(v)
	if err != nil {
		return false
	}
	switch {
	case p.Addr().Is4():
		return p.Bits() >= 8
	case p.Addr().Is4In6():
		// ::ffff:a.b.c.d/n covers IPv4 addresses, n-96 bits of them fixed.
		return p.Bits()-96 >= 8
	default:
		return p.Bits() >= 32
	}
}

// stringsOf decodes a policy value that is one value or an array of them.
// Numbers and booleans, which a condition may hold, are their JSON text.
func stringsOf(raw json.RawMessage) ([]string, error) {
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err != nil {
		many = []json.RawMessage{raw}
	}
	out := make([]string, 0, len(many))
	for _, r := range many {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			out = append(out, s)
			continue
		}
		var scalar any
		if err := json.Unmarshal(r, &scalar); err != nil {
			return nil, fmt.Errorf("decode a value: %w", err)
		}
		switch scalar.(type) {
		case float64, bool:
			out = append(out, string(r))
		default:
			return nil, fmt.Errorf("a value of type %T", scalar)
		}
	}
	return out, nil
}
