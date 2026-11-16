package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ArchitectureUsageError is a request that the architecture model cannot answer as asked: a format, a view or a
// topic that is not known. It names the values it takes.
type ArchitectureUsageError struct {
	Reason string
}

func (e *ArchitectureUsageError) Error() string { return e.Reason }

// FactList is the list of the compliance facts of one topic (ST-GEN-QRY06), in the key order of its JSON.
type FactList struct {
	Topic   string       `json:"topic"`
	Service string       `json:"service"`
	Model   string       `json:"model"`
	Record  string       `json:"record"`
	Scope   string       `json:"scope,omitempty"`
	Facts   []FactOfList `json:"facts"`
}

// FactOfList is one fact of the list: the fact of the model, the rule that sets it when it is a default, and the
// text and grant paths of the model that tell more about it.
type FactOfList struct {
	ID          string       `json:"id"`
	About       string       `json:"about"`
	Name        string       `json:"name"`
	Value       any          `json:"value"`
	State       string       `json:"state"`
	DefaultRule string       `json:"default_rule,omitempty"`
	Source      ArchSource   `json:"source"`
	Quoted      []QuotedText `json:"quoted,omitempty"`
	Grants      *FactGrants  `json:"grants,omitempty"`
	// GrantsUnknown is true when the check names a permission and the model holds no grant path of it.
	GrantsUnknown bool `json:"grants_unknown,omitempty"`
	// AncestorsIncomplete names, on the checks fact of an operation, each ancestor read whose grant paths a limit cut
	// short, with the fact of that ancestor check: more roles may hold the read than its paths name (security review
	// of PR #374, L2).
	AncestorsIncomplete []FactAncestor `json:"ancestors_incomplete,omitempty"`
}

// FactAncestor is an ancestor read of an operation: its fact and the permission it decides.
type FactAncestor struct {
	Fact       string `json:"fact"`
	Permission string `json:"permission"`
}

// QuotedText is free text of the declaration, such as the reason that an operation is public. It is shown as a
// quotation of its source, never as text of STEGO (ST-GEN-UC16-R03).
type QuotedText struct {
	Name   string     `json:"name"`
	Text   string     `json:"text"`
	Source ArchSource `json:"source"`
}

// FactGrants is who the permission of an access check grants: each grant path of the access matrix (ST-GEN-UC08-R13),
// and whether the paths are all of them.
type FactGrants struct {
	Permission string      `json:"permission"`
	Complete   bool        `json:"complete"`
	Paths      []ArchGrant `json:"paths"`
	// MayHold names, when a limit cut the paths short, each role of the access matrix that no path names: the role
	// may hold the permission by a path past the limit, as the matrix cell of that role says (security review of PR
	// #383, L1).
	MayHold []string `json:"may_hold,omitempty"`
}

// factGrants gives who the permission grants, with the roles of the model that may hold it past a limit.
func factGrants(m *ArchitectureModel, p ArchPermission) *FactGrants {
	g := &FactGrants{Permission: p.ID, Complete: p.Complete, Paths: p.Paths}
	if permissionIncomplete(&p) {
		for _, r := range m.Access.Roles {
			if len(matrixCell(&p, r.ID)) == 0 {
				g.MayHold = append(g.MayHold, r.ID)
			}
		}
	}
	return g
}

// ListComplianceFacts answers ST-GEN-QRY06: it reads the architecture model of the project folder through
// ReadArchitecture, and gives the facts of one topic as text, or as one JSON object, with every value from the model
// escaped so that no control or bidirectional character reaches the terminal. It writes nothing.
func ListComplianceFacts(projectDir, topic string, asJSON bool) ([]byte, error) {
	if !slices.Contains(ComplianceTopics, topic) {
		return nil, &ArchitectureUsageError{Reason: fmt.Sprintf("the topic %s is not known; the topics are %s", strconv.Quote(topic), strings.Join(ComplianceTopics, ", "))}
	}
	read, err := ReadArchitecture(projectDir)
	if err != nil {
		return nil, err
	}
	list := complianceFacts(&read.Model, topic)
	if asJSON {
		return factListJSON(list)
	}
	return []byte(factListText(list)), nil
}

// accessScope is the limit of the access topic (security review of #343, L2): it lists the checks of the declared
// operations and actions alone.
const accessScope = "The list covers only the operations, commands, queries and workers that the declaration states. An endpoint that a component serves by itself, such as a health endpoint, has no check record and is not in it."

// complianceFacts projects the facts of one topic out of the model alone (ST-GEN-UC16-R05): each fact as the model
// holds it, the rule of each default, the reason of each public check, and the grant paths of each permission that a
// check names.
func complianceFacts(m *ArchitectureModel, topic string) FactList {
	list := FactList{Topic: topic, Service: m.System.Name, Model: ArchitectureFile, Record: m.Record.Path, Facts: []FactOfList{}}
	if topic == "access" {
		list.Scope = accessScope
	}
	checks := map[string]ArchCheck{}
	// ancestors gives each ancestor check by its operation and the name of its fact.
	ancestors := map[string]ArchAncestor{}
	for _, c := range m.Access.Checks {
		checks[c.Subject] = c
		for n, a := range c.Ancestors {
			ancestors[c.Subject+"|"+ancestorFactName(n+1, a)] = a
		}
	}
	permissions := map[string]ArchPermission{}
	for _, p := range m.Access.Permissions {
		permissions[p.ID] = p
	}
	for _, t := range m.Compliance {
		if t.ID != "topic:"+topic {
			continue
		}
		for _, f := range t.Facts {
			item := FactOfList{ID: f.ID, About: f.About, Name: f.Name, Value: f.Value, State: f.State, Source: f.Source}
			if f.State == "default" {
				item.DefaultRule = f.Source.Default
			}
			if topic == "access" && f.Name == "checks" {
				if c, ok := checks[f.About]; ok {
					if c.Public && c.Reason != "" {
						item.Quoted = append(item.Quoted, QuotedText{Name: "reason", Text: c.Reason, Source: c.Source})
					}
					if p, ok := permissions[c.Grants]; ok {
						item.Grants = factGrants(m, p)
					} else if !c.Public && c.Permission != "" {
						item.GrantsUnknown = true
					}
					for n, a := range c.Ancestors {
						if p, ok := permissions[a.Grants]; ok && !p.Complete {
							item.AncestorsIncomplete = append(item.AncestorsIncomplete, FactAncestor{Fact: ancestorFactName(n+1, a), Permission: p.ID})
						}
					}
				}
			}
			if a, ok := ancestors[f.About+"|"+f.Name]; ok && topic == "access" {
				// Only the reason of a public read is free text of the declaration; the other reasons are STEGO's.
				if a.Public && a.Reason != "" {
					item.Quoted = append(item.Quoted, QuotedText{Name: "reason", Text: a.Reason, Source: a.Source})
				}
				if p, ok := permissions[a.Grants]; ok {
					item.Grants = factGrants(m, p)
				} else if a.Permission != "" {
					item.GrantsUnknown = true
				}
			}
			list.Facts = append(list.Facts, item)
		}
	}
	return list
}

// factListText gives the list as text: the facts grouped by what they are about, in the order of the model, each
// with its value, its state and its source. STEGO's own words in it say only what the inputs state.
func factListText(list FactList) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Facts of the topic %s for the service %s\n", list.Topic, nameText(list.Service))
	fmt.Fprintf(&b, "From %s, which the record %s lists.\n", list.Model, nameText(list.Record))
	if list.Scope != "" {
		b.WriteString(list.Scope + "\n")
	}
	b.WriteString("Each fact is what the inputs state, with its source. A person decides what the facts mean.\n")
	if len(list.Facts) == 0 {
		b.WriteString("\nThe model holds no fact of this topic.\n")
	}
	for i, f := range list.Facts {
		if i == 0 || f.About != list.Facts[i-1].About {
			fmt.Fprintf(&b, "\n%s\n", nameText(f.About))
		}
		switch f.State {
		case "unstated":
			fmt.Fprintf(&b, "  %s: unstated. The inputs do not state it.\n", nameText(f.Name))
			fmt.Fprintf(&b, "    source: %s\n", factSourceText(f.Source))
		case "default":
			fmt.Fprintf(&b, "  %s: %s\n", nameText(f.Name), textValue(f.Value))
			if f.DefaultRule != "" {
				fmt.Fprintf(&b, "    default, set by the rule %s\n", nameText(f.DefaultRule))
			} else {
				fmt.Fprintf(&b, "    default, source: %s\n", factSourceText(f.Source))
			}
		default:
			fmt.Fprintf(&b, "  %s: %s\n", nameText(f.Name), textValue(f.Value))
			fmt.Fprintf(&b, "    source: %s\n", factSourceText(f.Source))
		}
		for _, q := range f.Quoted {
			fmt.Fprintf(&b, "    %s, quoted from %s: %s\n", nameText(q.Name), factSourceText(q.Source), quoted(q.Text))
		}
		if f.Grants != nil {
			writeGrants(&b, f.Grants)
		}
		if f.GrantsUnknown {
			fmt.Fprintf(&b, "    the grants of %s are not known: the model holds no grant path of it\n", textValue(f.Value))
		}
		for _, a := range f.AncestorsIncomplete {
			fmt.Fprintf(&b, "    a limit cut a path of the ancestor read %s short (%s), so it can grant more roles than its paths name\n", nameText(a.Permission), nameText(a.Fact))
		}
	}
	return b.String()
}

// writeGrants writes who the permission of a check grants, one line for each grant path with the source of its schema.
func writeGrants(b *strings.Builder, g *FactGrants) {
	if len(g.Paths) == 0 && g.Complete {
		fmt.Fprintf(b, "    %s has no grant path, so no role holds it\n", nameText(g.Permission))
	}
	for _, p := range g.Paths {
		line := "    granted to " + nameText(p.Role)
		if p.Role == "" {
			line = "    a path that reaches no role"
		}
		line += fmt.Sprintf(", by a path of kind %s (%s)", nameText(strings.Join(p.Kinds, ", ")), nameText(strings.Join(p.Hops, " > ")))
		if p.Caveat != "" {
			line += ", with the caveat " + nameText(p.Caveat)
		}
		if len(p.Requires) > 0 {
			line += ", which also requires " + nameText(strings.Join(p.Requires, ", "))
		}
		if len(p.Except) > 0 {
			line += ", except " + nameText(strings.Join(p.Except, ", "))
		}
		if p.Stops != "" {
			line += ", stopped: " + nameText(p.Stops)
		}
		b.WriteString(line + "; source: " + factSourceText(p.Source) + "\n")
	}
	if !g.Complete {
		fmt.Fprintf(b, "    a limit cut a path of %s short, so it can grant more roles than these\n", nameText(g.Permission))
		if len(g.MayHold) > 0 {
			fmt.Fprintf(b, "    no path that the model names gives it to %s, but each may hold it by a path past the limit\n", nameText(strings.Join(g.MayHold, ", ")))
		}
	}
}

// factSourceText names the source of a fact in words.
func factSourceText(s ArchSource) string {
	switch {
	case s.Declaration != "":
		return "the declaration at " + nameText(s.Declaration)
	case s.Registry != nil:
		text := fmt.Sprintf("the registry part %s %s %s", nameText(s.Registry.Part), nameText(s.Registry.Name), nameText(s.Registry.Version))
		if s.Registry.Field != "" {
			text += ", field " + nameText(s.Registry.Field)
		}
		return text
	case s.Wiring != "":
		return "the wiring at " + nameText(s.Wiring)
	case s.Default != "":
		return "the default rule " + nameText(s.Default)
	case s.Rule != "":
		return "the rule " + nameText(s.Rule) + " of STEGO"
	}
	return "no source"
}

// plainValue is a value of the model that is shown as it is: a name, a path or a number. Any other text is quoted.
var plainValue = regexp.MustCompile(`^[A-Za-z0-9_.:#/@+-]+(, [A-Za-z0-9_.:#/@+-]+)*$`)

// claimWords are the words that would say that a service complies with something (ST-GEN-UC16-R03). STEGO's own text
// never holds them; a value of the model that holds one is quoted.
var claimWords = regexp.MustCompile(`(?i)\b(complian\w*|complie[sd]|comply\w*|pass\w*|meet\w*|met|fail\w*|satisf\w*|conform\w*|certif\w*|violat\w*|secure[sd]?|safe(ly)?|protected|approv\w*)\b`)

// textValue gives a value of a fact: a plain name or number as it is, and other text quoted.
func textValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "none"
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case json.Number:
		return printable(v.String())
	case string:
		if plainValue.MatchString(v) && !claimWords.MatchString(v) {
			return v
		}
		return quoted(v) + " (quoted)"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "a value that cannot be shown"
	}
	return quoted(string(data)) + " (quoted)"
}

// nameText gives a name of the model, such as an element ID or a declaration path, as it is, escaped by printable;
// a name that holds a claim word is quoted, so STEGO's own text never holds one (ST-GEN-UC16-R03).
func nameText(text string) string {
	if claimWords.MatchString(text) {
		return quoted(text) + " (quoted)"
	}
	return printable(text)
}

// quoted gives text of the model in double quotes, with each quote, backslash, control and bidirectional character
// escaped.
func quoted(text string) string {
	return `"` + escapeRunes(text, func(r rune) bool { return r == '"' || r == '\\' || unsafeRune(r) }, backslashEscape) + `"`
}

// printable gives text of the model with each control and bidirectional character, and each byte that is not
// UTF-8, escaped as \uXXXX or \xXX, so that the text cannot move the cursor, change colors or reorder a line on a
// terminal (security review of #338).
func printable(text string) string {
	return escapeRunes(text, unsafeRune, backslashEscape)
}

// unsafeRune is a rune that a terminal does not show as itself: a control, a format character such as a
// bidirectional override, or a line or paragraph separator.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || r == utf8.RuneError
}

// backslashEscape escapes a rune as \uXXXX, or as a pair of them above the basic plane.
func backslashEscape(r rune) string {
	if r > 0xFFFF {
		a, b := utf16.EncodeRune(r)
		return fmt.Sprintf(`\u%04x\u%04x`, a, b)
	}
	return fmt.Sprintf(`\u%04x`, r)
}

// escapeRunes gives text with each rune that unsafe takes in the form escape gives it, and each byte that is not
// UTF-8 as \xXX.
func escapeRunes(text string, unsafe func(rune) bool, escape func(rune) string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, text[i])
		case unsafe(r):
			b.WriteString(escape(r))
		default:
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

// factListJSON gives the list as one JSON object with its keys in a fixed order, two-space indent and one trailing
// newline, with each control and bidirectional character of a string escaped.
func factListJSON(list FactList) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(list); err != nil {
		return nil, err
	}
	// Outside its strings a JSON text holds only ASCII, so each rune that the encoder leaves as it is and that is
	// not safe on a terminal is in a string, where \uXXXX is the same character.
	return []byte(escapeRunes(buffer.String(), func(r rune) bool { return r != '\n' && unsafeRune(r) }, backslashEscape)), nil
}
