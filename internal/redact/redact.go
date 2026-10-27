// Package redact hides from log lines what the logs API must not hand out:
// secrets GameFleet knows by value, anything shaped like a credential, and
// players' addresses and email addresses. Addresses and email addresses
// become pseudonyms, the same for the same value while GameFleet runs, so
// that a reader still sees one client connect twice without learning who it
// is. Player IDs stay: they are random, and the only way to follow a match.
package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLine is how long a line may be; the rest is cut.
const MaxLine = 2000

// Secret is a value GameFleet knows that no log may show, and what to show
// instead, for example "runtime:billiards:TICKET_KEY".
type Secret struct {
	Name, Value string
}

// minSecret is the shortest value hidden by value: shorter ones, such as a
// port or "true", would hide what is no secret.
const minSecret = 8

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	jwt  = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`)
	// The credential after an HTTP authorization scheme.
	scheme = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// A value named like a credential: password=…, "token": "…", ?key=…,
	// secret='…'. Any name with "key" in it counts, though it hides some
	// that are none. A quoted value is hidden to its closing quote, past
	// escaped ones, and one with no closing quote to the end of the line. A
	// known secret's marker in it stays, so that it shows its name.
	named  = regexp.MustCompile(`(?i)([a-z0-9_.-]*(?:password|passwd|secret|token|key|credential|signature|ticket)[a-z0-9_.-]*['"]?\s*[=:]\s*)('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|['"].+|[^\s"',;&}]+)`)
	marker = regexp.MustCompile("\x00[0-9]+\x00")
	// The same as a command line flag: --token …, -password ….
	flag  = regexp.MustCompile(`(?i)(\s--?[a-z0-9_.-]*(?:password|passwd|secret|token|key|credential|signature|ticket)[a-z0-9_.-]*\s+)([^\s\x00-]\S*)`)
	email = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	ipv4  = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	// Candidates only: netip decides what is an IPv6 address.
	ipv6 = regexp.MustCompile(`(?i)[0-9a-f]*:[0-9a-f:]*:[0-9a-f:.]*`)
)

// Redactor redacts with one key for pseudonyms, made when GameFleet starts.
type Redactor struct {
	key []byte
}

// New returns a redactor with a key of its own.
func New() *Redactor {
	key := make([]byte, 32)
	rand.Read(key)
	return &Redactor{key: key}
}

// Pass redacts lines with a set of secrets. Make one per request: the
// secrets change when someone edits the runtime settings.
type Pass struct {
	r *Redactor
	// A secret's value first becomes a marker that no rule matches, and its
	// name only at the end: a name such as [runtime:chess:API_TOKEN] looks
	// like a credential itself.
	markers, names *strings.Replacer
}

// With returns a pass that also hides the secrets' values.
func (r *Redactor) With(secrets []Secret) *Pass {
	// A value of several lines, such as a PEM key, also line by line: a pass
	// sees one line at a time.
	var list []Secret
	for _, s := range secrets {
		list = append(list, s)
		if strings.Contains(s.Value, "\n") {
			for _, line := range strings.Split(s.Value, "\n") {
				list = append(list, Secret{Name: s.Name, Value: strings.TrimSpace(line)})
			}
		}
	}
	// Longer values first, so that a secret containing another is hidden whole.
	slices.SortFunc(list, func(a, b Secret) int { return len(b.Value) - len(a.Value) })
	var markers, names []string
	for i, s := range list {
		if len(s.Value) >= minSecret {
			marker := fmt.Sprintf("\x00%d\x00", i)
			markers = append(markers, s.Value, marker)
			names = append(names, marker, "["+s.Name+"]")
		}
	}
	return &Pass{r: r, markers: strings.NewReplacer(markers...), names: strings.NewReplacer(names...)}
}

// Text redacts each line of text.
func (p *Pass) Text(text string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body, newline := strings.CutSuffix(line, "\n")
		lines[i] = p.Line(body)
		if newline {
			lines[i] += "\n"
		}
	}
	return strings.Join(lines, "")
}

// Line redacts one line.
func (p *Pass) Line(line string) string {
	// No marker is left in line: printable drops the NULs they are made of.
	line = printable(line)
	line = p.markers.Replace(line)
	line = jwt.ReplaceAllString(line, "[token]")
	line = scheme.ReplaceAllString(line, "$1$2[hidden]")
	line = named.ReplaceAllStringFunc(line, hideNamed)
	line = flag.ReplaceAllString(line, "$1[hidden]")
	line = email.ReplaceAllStringFunc(line, func(s string) string { return p.r.pseudonym("email", strings.ToLower(s)) })
	line = ipv4.ReplaceAllStringFunc(line, p.address)
	line = ipv6.ReplaceAllStringFunc(line, p.address)
	line = p.names.Replace(line)
	if len(line) > MaxLine {
		cut := MaxLine
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + " …[cut]"
	}
	return line
}

// hideNamed hides the value of a named match, but its quotes and the
// markers of known secrets in it.
func hideNamed(match string) string {
	m := named.FindStringSubmatch(match)
	name, value := m[1], m[2]
	var open, close string
	if q := value[:1]; q == "'" || q == `"` {
		open, value = q, value[1:]
		if strings.HasSuffix(value, q) {
			close, value = q, value[:len(value)-1]
		}
	}
	var b strings.Builder
	b.WriteString(name + open)
	last := 0
	for _, at := range marker.FindAllStringIndex(value, -1) {
		if at[0] > last {
			b.WriteString("[hidden]")
		}
		b.WriteString(value[at[0]:at[1]])
		last = at[1]
	}
	if last < len(value) {
		b.WriteString("[hidden]")
	}
	b.WriteString(close)
	return b.String()
}

// address replaces a public IP address with its pseudonym. Private,
// loopback and other addresses that are no one's stay: they name machines
// and pods of the cluster.
func (p *Pass) address(s string) string {
	addr, err := netip.ParseAddr(strings.TrimSuffix(s, ":"))
	if err != nil || !public(addr) {
		return s
	}
	out := p.r.pseudonym("ip", addr.Unmap().String())
	if strings.HasSuffix(s, ":") {
		out += ":"
	}
	return out
}

// cgnat is shared address space, used inside providers and overlay networks.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func public(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

func (r *Redactor) pseudonym(kind, value string) string {
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(kind + "\x00" + value))
	return kind + "#" + hex.EncodeToString(mac.Sum(nil)[:3])
}

// printable drops terminal escapes and control characters but tabs, so that
// a log line cannot move a reader's cursor or hide what follows it.
func printable(s string) string {
	s = ansi.ReplaceAllString(s, "")
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r != '\t' && unicode.IsControl(r) }
