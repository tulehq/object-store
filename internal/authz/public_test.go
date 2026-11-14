package authz

import (
	"testing"

	"tule.dev/policy"
)

// The cases are secret-store's for the same rule (its public_test.go), on
// S3 statements, with S3's access point keys added.

const partnerARN = "arn:aws:iam::111122223333:user/partner"

func stmt(effect, principal, condition string) string {
	s := `{"Effect":"` + effect + `","Principal":` + principal +
		`,"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"`
	if condition != "" {
		s += `,"Condition":` + condition
	}
	return s + `}`
}

func doc(stmts ...string) string {
	out := `{"Version":"2012-10-17","Statement":[`
	for i, s := range stmts {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out + `]}`
}

var publicCases = []struct {
	name   string
	doc    string
	public bool
}{
	{"star", doc(stmt("Allow", `"*"`, "")), true},
	{"AWS star", doc(stmt("Allow", `{"AWS":"*"}`, "")), true},
	{"star among others", doc(stmt("Allow", `{"AWS":["`+partnerARN+`","*"]}`, "")), true},
	{"single statement object", `{"Version":"2012-10-17","Statement":` +
		stmt("Allow", `"*"`, "") + `}`, true},
	{"fixed principal", doc(stmt("Allow", `{"AWS":"`+partnerARN+`"}`, "")), false},
	{"account", doc(stmt("Allow", `{"AWS":"111122223333"}`, "")), false},
	{"deny star", doc(stmt("Deny", `"*"`, "")), false},
	{"org ID", doc(stmt("Allow", `"*"`,
		`{"StringEquals":{"aws:PrincipalOrgID":"o-abc123"}}`)), false},
	{"key in another case", doc(stmt("Allow", `"*"`,
		`{"StringEquals":{"AWS:principalorgid":"o-abc123"}}`)), false},
	{"escaped key", doc(stmt("Allow", `"*"`,
		`{"StringEquals":{"aws:PrincipalOrgID":"o-abc123"}}`)), false},
	{"wildcard value", doc(stmt("Allow", `"*"`,
		`{"StringLike":{"aws:PrincipalOrgID":"o-*"}}`)), true},
	{"policy variable", doc(stmt("Allow", `"*"`,
		`{"StringEquals":{"aws:PrincipalAccount":"${aws:username}"}}`)), true},
	{"IfExists", doc(stmt("Allow", `"*"`,
		`{"StringEqualsIfExists":{"aws:SourceVpc":"vpc-1"}}`)), true},
	{"negated", doc(stmt("Allow", `"*"`,
		`{"StringNotEquals":{"aws:SourceVpc":"vpc-1"}}`)), true},
	{"ForAllValues", doc(stmt("Allow", `"*"`,
		`{"ForAllValues:StringEquals":{"aws:SourceVpce":"vpce-1"}}`)), true},
	{"ForAnyValue", doc(stmt("Allow", `"*"`,
		`{"ForAnyValue:StringEquals":{"aws:SourceVpce":"vpce-1"}}`)), false},
	{"other key", doc(stmt("Allow", `"*"`,
		`{"Bool":{"aws:SecureTransport":"true"}}`)), true},
	{"narrow range", doc(stmt("Allow", `"*"`,
		`{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","192.0.2.1"]}}`)), false},
	{"broad range", doc(stmt("Allow", `"*"`,
		`{"IpAddress":{"aws:SourceIp":"0.0.0.0/1"}}`)), true},
	{"broad mapped range", doc(stmt("Allow", `"*"`,
		`{"IpAddress":{"aws:SourceIp":"::ffff:0.0.0.0/97"}}`)), true},
	{"role sessions by user ID", doc(stmt("Allow", `"*"`,
		`{"StringLike":{"aws:userid":"AROAEXAMPLEROLE00001:*"}}`)), false},
	{"one broad statement of two", doc(
		stmt("Allow", `{"AWS":"`+partnerARN+`"}`, ""), stmt("Allow", `"*"`, "")), true},
	{"access point, fixed", doc(stmt("Allow", `"*"`,
		`{"StringEquals":{"s3:DataAccessPointArn":`+
			`"arn:aws:s3:us-west-2:123456789012:accesspoint/ap"}}`)), false},
	{"access point name wildcard", doc(stmt("Allow", `"*"`,
		`{"StringLike":{"s3:DataAccessPointArn":`+
			`"arn:aws:s3:us-west-2:123456789012:accesspoint/*"}}`)), false},
	{"access point account wildcard", doc(stmt("Allow", `"*"`,
		`{"StringLike":{"s3:DataAccessPointArn":"arn:aws:s3:us-west-2:*:accesspoint/*"}}`)), true},
	{"access point account", doc(stmt("Allow", `"*"`,
		`{"StringEquals":{"s3:DataAccessPointAccount":"123456789012"}}`)), false},
	{
		"service principal", doc(stmt("Allow", `{"Service":"cloudtrail.amazonaws.com"}`, "")),
		false,
	},
}

func TestPublicPolicy(t *testing.T) {
	for _, c := range publicCases {
		if _, err := policy.ParseResource([]byte(c.doc)); err != nil {
			t.Fatalf("%s: the case does not parse: %v", c.name, err)
		}
		public, err := PublicPolicy([]byte(c.doc))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if public != c.public {
			t.Errorf("%s: public = %v, want %v", c.name, public, c.public)
		}
	}
	if _, err := PublicPolicy([]byte(`{"Statement":"x"}`)); err == nil {
		t.Error("a document that does not decode was reported")
	}
}
