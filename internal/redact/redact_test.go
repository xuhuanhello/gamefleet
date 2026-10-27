package redact

import (
	"strings"
	"testing"
)

func TestLineHidesSecretsAndPeople(t *testing.T) {
	p := New().With([]Secret{
		{Name: "runtime:billiards:TICKET_KEY", Value: "s3cr3t-ticket-key-value"},
		{Name: "runtime:billiards:DEBUG", Value: "true"},
		{Name: "runtime:token-game:API_TOKEN", Value: "another-secret-value"},
	})
	cases := map[string]struct{ in, want string }{
		"a secret by value": {
			"signing with s3cr3t-ticket-key-value now", "signing with [runtime:billiards:TICKET_KEY] now"},
		"a short value is no secret": {"DEBUG=true", "DEBUG=true"},
		"a name that looks like a credential": {
			"key another-secret-value, NUL \x00 0 \x00", "key [runtime:token-game:API_TOKEN], NUL  0 "},
		"a JWT": {
			"session eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOiJ1LTEifQ.c2lnbmF0dXJl ok", "session [token] ok"},
		"a bearer header": {"Authorization: Bearer 0123456789abcdef", "Authorization: Bearer [hidden]"},
		"a named value":   {"login password=hunter22 user=u-1", "login password=[hidden] user=u-1"},
		"a JSON value":    {`{"ticket":"t-abc.def","matchId":"m-1"}`, `{"ticket":"[hidden]","matchId":"m-1"}`},
		"a query value":   {"GET /join?token=abc123&room=r-1", "GET /join?token=[hidden]&room=r-1"},
		"a flag":          {"k3s agent --token K10abc::server:x --server https://10.4.0.4:6443", "k3s agent --token [hidden] --server https://10.4.0.4:6443"},
		"private addresses stay": {
			"dial 10.4.0.2:5432 and 127.0.0.1 and 172.20.1.3 and 100.64.0.9", "dial 10.4.0.2:5432 and 127.0.0.1 and 172.20.1.3 and 100.64.0.9"},
		"times and versions stay": {"at 12:34:56 on k3s v1.35.8", "at 12:34:56 on k3s v1.35.8"},
		"player IDs stay": {
			"player 4ef51ac5-ae18-4ec3-83b7-9d64a95b2ac1 seat 0", "player 4ef51ac5-ae18-4ec3-83b7-9d64a95b2ac1 seat 0"},
		"escapes go": {"\x1b[31mred\x1b[0m\rover\x07", "redover"},
	}
	for name, c := range cases {
		if got := p.Line(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

func TestPseudonymsKeepWhoIsWho(t *testing.T) {
	p := New().With(nil)
	a := p.Line("client 198.51.100.7:50123 connected")
	b := p.Line("client 198.51.100.7:50999 left")
	c := p.Line("client 198.51.100.8:50123 connected")
	if strings.Contains(a+b+c, "198.51.100") {
		t.Fatalf("addresses shown: %q %q %q", a, b, c)
	}
	pseudonym := func(line string) string { return strings.Fields(line)[1][:len("ip#")+6] }
	if pseudonym(a) != pseudonym(b) || pseudonym(a) == pseudonym(c) {
		t.Fatalf("pseudonyms: %q %q %q", a, b, c)
	}
	if !strings.Contains(a, ":50123") {
		t.Errorf("port lost: %q", a)
	}
	v6 := p.Line("from [2001:db8:85a3::8a2e:370:7334]:443 and fe80::1 and ::1")
	if strings.Contains(v6, "2001:db8") || !strings.Contains(v6, "fe80::1") || !strings.Contains(v6, "::1") {
		t.Errorf("IPv6: %q", v6)
	}
	mail := p.Line("sent code to Alice@Example.com and alice@example.com")
	fields := strings.Fields(mail)
	if strings.Contains(mail, "example") || fields[3] != fields[5] {
		t.Errorf("email: %q", mail)
	}
	// Another run of GameFleet gives other pseudonyms.
	if New().With(nil).Line("client 198.51.100.7:50123 connected") == a {
		t.Error("two redactors gave the same pseudonym")
	}
}

func TestTextKeepsLinesAndCutsLongOnes(t *testing.T) {
	p := New().With(nil)
	long := strings.Repeat("界", MaxLine)
	got := p.Text("one password=x1\n" + long + "\n")
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || lines[0] != "one password=[hidden]" || lines[2] != "" {
		t.Fatalf("lines = %q", lines)
	}
	if len(lines[1]) > MaxLine+len(" …[cut]") || !strings.HasSuffix(lines[1], " …[cut]") || !strings.HasPrefix(lines[1], "界") {
		t.Errorf("long line: %d bytes, ends %q", len(lines[1]), lines[1][len(lines[1])-12:])
	}
}

// A secret of several lines is hidden on each line it is printed on.
func TestTextHidesASecretOfSeveralLines(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIBx3J0m0v9n4p1y6ZqXc7gHkQe2ZtVfG\n-----END PRIVATE KEY-----\n"
	p := New().With([]Secret{{Name: "runtime:billiards:TICKET_PRIVATE", Value: pem}})
	got := p.Text("loading key\n" + strings.ReplaceAll(pem, "\n", "\r\n") + "done\n")
	if strings.Contains(got, "MC4CAQAw") || strings.Contains(got, "BEGIN PRIVATE") {
		t.Fatalf("the key shows:\n%s", got)
	}
	if !strings.Contains(got, "loading key\n") || !strings.Contains(got, "[runtime:billiards:TICKET_PRIVATE]") || !strings.HasSuffix(got, "done\n") {
		t.Fatalf("got:\n%s", got)
	}
}

// Values named like credentials that GameFleet does not know are hidden too.
func TestLineHidesUnknownNamedValues(t *testing.T) {
	p := New().With(nil)
	cases := map[string]string{
		"connect key=0f3a9c2e77 to us":       "connect key=[hidden] to us",
		"secret='abc def' left":              "secret='[hidden]' left",
		`opened "token": "half`:              `opened "token": "[hidden]`,
		"secret='alpha beta":                 "secret='[hidden]",
		`secret="alpha\"beta" next`:          `secret="[hidden]" next`,
		`config {"server_key": "zz-1234"}`:   `config {"server_key": "[hidden]"}`,
		"token = 'k10abc' and matchId=m-1":   "token = '[hidden]' and matchId=m-1",
		"run --api-key sk-12345 --region us": "run --api-key [hidden] --region us",
	}
	for in, want := range cases {
		if got := p.Line(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

// A known secret in a named value keeps its name.
func TestANamedKnownSecretKeepsItsName(t *testing.T) {
	p := New().With([]Secret{{Name: "runtime:billiards:TICKET_KEY", Value: "s3cr3t-ticket-key-value"}})
	for in, want := range map[string]string{
		"key=s3cr3t-ticket-key-value ok":      "key=[runtime:billiards:TICKET_KEY] ok",
		`{"key":"s3cr3t-ticket-key-value"}`:   `{"key":"[runtime:billiards:TICKET_KEY]"}`,
		"token='s3cr3t-ticket-key-value tail": "token='[runtime:billiards:TICKET_KEY][hidden]",
	} {
		if got := p.Line(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}
