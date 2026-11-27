package assure

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The tests of the rules of ST-STD-TXN that a scan of the store code can hold: the tenant setting (R02), the roles
// (R03), the audit table grants (R06), money (R12) and the claim of rows (R14). Each reads the store code of the tree
// at HEAD, the Go files that are not test files, the templates of generated Go and the SQL files of the runtime
// library and of the generators, outside testdata; so run them with -count=1 after a commit. Each scan proves only
// the part of its rule that the text shows; the record of each test names the part that it does not prove.

// storeText is one file of store code, as text with its comment lines blanked.
type storeText struct {
	name string
	text string
}

// storeTexts gives each file of store code of the runtime library and the generators.
func storeTexts(t *testing.T, tree Tree) []storeText {
	t.Helper()
	var files []storeText
	for _, name := range tree.paths() {
		if !strings.HasPrefix(name, runtimeModule+"/") && !strings.HasPrefix(name, "internal/generator/") || strings.Contains(name, "/testdata/") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") || !slices.ContainsFunc([]string{".go", ".go.tmpl", ".sql", ".sql.tmpl"}, func(ext string) bool { return strings.HasSuffix(name, ext) }) {
			continue
		}
		data, _ := tree.File(name)
		files = append(files, storeText{name: name, text: withoutLineComments(string(data))})
	}
	if len(files) == 0 {
		t.Fatal("the tree holds no store code")
	}
	return files
}

// withoutLineComments blanks each line that is a Go or SQL comment, so a statement in a comment does not count.
func withoutLineComments(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// lineOf gives the line of the text at the offset, trimmed, for a fault.
func lineOf(text string, offset int) string {
	start := strings.LastIndex(text[:offset], "\n") + 1
	end := strings.Index(text[offset:], "\n")
	if end < 0 {
		end = len(text) - offset
	}
	return strings.TrimSpace(text[start : offset+end])
}

// A claim of rows: a SELECT that takes a batch of rows to work on, with a LIMIT or a FETCH FIRST and a row lock (FOR
// UPDATE, FOR NO KEY UPDATE, FOR SHARE or FOR KEY SHARE) at the same level of the statement, in either order (the
// lock before the limit, FETCH FIRST and FOR SHARE are from the security review of PR #504, L3). A lock of one row by its key (FOR UPDATE with
// no LIMIT) is not a claim. The scan reads the words of the text (sqlWords): it reads across lines, comments, quoted
// names, Go string joins and any limit value, such as LIMIT 32, LIMIT $1, LIMIT (n), LIMIT @n, LIMIT %d or LIMIT " +
// strconv.Itoa(n) + ", and an OFFSET after the limit (security review of the store scans, sec-10). It is not a SQL
// parser: a claim whose LIMIT is more than claimReach words from its lock is not found. The lock is found in the words
// too, in any case, so a lock across a Go join or a comment, such as FOR ` + `UPDATE or FOR/**/UPDATE, is found, and a
// LIMIT of a subquery or of a WITH query of the locking SELECT makes it a claim (security review of PR #14, L3).

// claimReach is how far, in words, the scan reads from a locking clause for its LIMIT and its statement.
const claimReach = 120

// sqlWord is one word of SQL text for the claim scan: a keyword or a name, or one of ( ) , . ; as is, or END. key is
// the word in upper case, text the word as the text has it, and at its offset in the text.
type sqlWord struct {
	key, text string
	at        int
}

// sqlWords gives the words of text: names and keywords, and each bracket, comma, dot and semicolon. It drops block
// comments, a -- comment that follows a space up to the end of its line, and each other character, such as quotes, backslashes and the + of a Go join, so OF "m" reads
// as OF m. In Go text (goText), the quote that closes a Go string literal, when a ), a comma, a semicolon, a //
// comment or the end of its line follows it, gives the word END, so the words of Go code after the SQL text, such as
// a comment // SKIP LOCKED or the argument limit, are not read as SQL (security review of PR #14, L3). Only the quote
// character that opened the literal closes it: a double quote in a raw string literal, such as OF "m" at the end of
// a line, is a quoted name of the SQL text and gives no END (security review sec-21-14c, L-14b). Outside a literal,
// the scan drops a // comment and a rune literal, so that a quote in them does not open a literal. SQL text (not goText), such as a .sql file, has no Go literal,
// so a quote in it gives no END.
func sqlWords(text string, goText bool) []sqlWord {
	var words []sqlWord
	var open byte // the quote character of the Go string literal that is open, or 0
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case goText && open == '"' && c == '\\':
			// An escape in a double quote literal, such as \" of a quoted name.
			i += 2
		case goText && open == 0 && c == '/' && i+1 < len(text) && text[i+1] == '/':
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				return words
			}
			i += end
		case goText && open == 0 && c == '\'' && i+2 < len(text) && text[i+2] == '\'':
			i += 3
		case goText && open == 0 && c == '\'' && i+3 < len(text) && text[i+1] == '\\' && text[i+3] == '\'':
			i += 4
		case goText && (c == '`' || c == '"') && (open == 0 || open == c):
			if open == 0 {
				open = c
				i++
				continue
			}
			open = 0
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\r') {
				j++
			}
			if j == len(text) || strings.IndexByte("),;\n", text[j]) >= 0 || strings.HasPrefix(text[j:], "//") {
				words = append(words, sqlWord{"END", string(c), i})
			}
			i++
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return words
			}
			i += end + 4
		case c == '-' && i+1 < len(text) && text[i+1] == '-' && (i == 0 || text[i-1] == ' ' || text[i-1] == '\t' || text[i-1] == '\n'):
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				return words
			}
			i += end
		case c == '{' && strings.TrimLeft(text[i+1:min(len(text), i+1+strings.IndexByte(text[i+1:]+"\n", '\n'))], " \t\r") == "",
			c == '}' && strings.TrimLeft(text[strings.LastIndexByte(text[:i], '\n')+1:i], " \t") == "":
			// A brace that ends or starts a line opens or closes a block of Go code: the end of a statement of
			// SQL text. A brace of a template name, such as {s}.{events}, is in a line.
			words = append(words, sqlWord{"BLOCK", string(c), i})
			i++
		case c == '(' || c == ')' || c == ',' || c == '.' || c == ';':
			words = append(words, sqlWord{string(c), string(c), i})
			i++
		case c == '_' || c == '$' || c == '@' || c == '%' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9':
			j := i
			for j < len(text) && (text[j] == '_' || text[j] == '$' || text[j] == '@' || text[j] == '%' || 'a' <= text[j] && text[j] <= 'z' || 'A' <= text[j] && text[j] <= 'Z' || '0' <= text[j] && text[j] <= '9') {
				j++
			}
			words = append(words, sqlWord{strings.ToUpper(text[i:j]), text[i:j], i})
			i = j
		default:
			i++
		}
	}
	return words
}

// limitBefore tells whether the words before a locking clause, read back to the start of its SELECT, hold a LIMIT of
// that SELECT, of a subquery of it or of a WITH query before it: a batch of rows that the lock takes, also when the
// LIMIT is in a subquery (decision on the security review of PR #14, L3). The read stops at a semicolon, at a block of
// Go code (BLOCK), at the end of an earlier Go string literal (END), at the open bracket of a subquery that holds the lock, and at the SELECT of the lock, unless the
// close bracket of a WITH query is before it, so the LIMIT of another statement does not count.
func limitBefore(words []sqlWord) bool {
	depth := 0
	for i := len(words) - 1; i >= 0; i-- {
		switch words[i].key {
		case ")":
			depth++
		case "(":
			if depth == 0 {
				return false
			}
			depth--
		case ";", "BLOCK", "END":
			return false
		case "SELECT":
			if depth == 0 && (i == 0 || words[i-1].key != ")") {
				return false
			}
		case "LIMIT", "FETCH":
			return true
		}
	}
	return false
}

// lockTail reads the words after a row lock: the list of names of OF, NOWAIT, SKIP LOCKED and more locking clauses,
// then a LIMIT or a FETCH of the same SELECT, which PostgreSQL also takes after the lock. It gives whether the tail
// holds SKIP LOCKED and whether a limit follows, in any case. The read stops at the end of the Go string literal (END),
// so the Go name limit in the arguments of a call after the SQL text does not count, and at any other word. At a
// close bracket, the lock is in a subquery or a WITH query, and a LIMIT of a SELECT that holds it (outerLimit) bounds
// the rows that it locks: that is a claim too (decision on the security review sec-21-14c, L-14c).
func lockTail(words []sqlWord) (skipLocked, limit bool) {
	names := false
	for i := 0; i < len(words); i++ {
		switch words[i].key {
		case "SKIP":
			if i+1 < len(words) && words[i+1].key == "LOCKED" {
				skipLocked = true
				i++
				names = false
				continue
			}
			return skipLocked, false
		case "FOR", "NO", "KEY", "UPDATE", "SHARE", "NOWAIT":
			names = false
		case "OF":
			// The names of the list of OF follow, each of one or more words, such as "my m" or stego_outbox.m.
			names = true
		case "LIMIT", "FETCH":
			return skipLocked, true
		case ",", ".":
			if !names {
				return skipLocked, false
			}
		case ")":
			return skipLocked, outerLimit(words[i+1:])
		default:
			if !names || slices.Contains([]string{"END", "BLOCK", "(", ")", ";"}, words[i].key) {
				return skipLocked, false
			}
		}
	}
	return skipLocked, false
}

// outerLimit tells whether the words after the close bracket of a subquery or a WITH query that holds a row lock
// hold a LIMIT or a FETCH of a SELECT that holds the bracket, such as WITH c AS (... FOR UPDATE) SELECT id FROM c
// LIMIT 10. PostgreSQL reads such a query only as far as the outer SELECT needs rows, so the outer LIMIT bounds the
// rows that the lock takes. A LIMIT in another bracket, such as of another WITH query or a subquery of a join, does
// not count. The read stops at a semicolon, at a block of Go code (BLOCK) and at the end of the Go string literal (END).
func outerLimit(words []sqlWord) bool {
	depth := 0
	for _, word := range words {
		switch word.key {
		case "(":
			depth++
		case ")":
			// At depth 0, the close bracket of a further SELECT that holds the lock.
			depth = max(0, depth-1)
		case ";", "BLOCK", "END":
			return false
		case "LIMIT", "FETCH":
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// lockAt gives the index of the word after the row lock (FOR UPDATE, FOR NO KEY UPDATE, FOR SHARE or FOR KEY SHARE)
// that starts at words[i], or -1 when no lock starts there.
func lockAt(words []sqlWord, i int) int {
	if words[i].key != "FOR" {
		return -1
	}
	for _, lock := range [][]string{{"UPDATE"}, {"NO", "KEY", "UPDATE"}, {"SHARE"}, {"KEY", "SHARE"}} {
		if i+len(lock) < len(words) && slices.EqualFunc(words[i+1:i+1+len(lock)], lock, func(w sqlWord, key string) bool { return w.key == key }) {
			return i + 1 + len(lock)
		}
	}
	return -1
}

// A session-scoped advisory lock: pg_advisory_lock, pg_try_advisory_lock and their _shared forms, with no _xact.
var sessionLock = regexp.MustCompile(`(?i)pg_(try_)?advisory_lock(_shared)?\s*\(`)

// sessionLockOwners are the files whose session-scoped advisory locks serialize work that claims no outbox or job
// row, which ST-STD-TXN-R14 names: the migration job on the owner connection (service/migrate.go, the schema steps of
// postgresadapter), and the provisioning of a database by an administrator (postgresclient/provision.go.tmpl).
var sessionLockOwners = []string{runtimeModule + "/service/migrate.go", "internal/generator/postgresadapter/generator.go",
	"internal/generator/postgresclient/provision.go.tmpl"}

// pagedRowLocks are the files whose LIMIT ... FOR UPDATE pages application rows of one tenant to change them, not a
// queue of outbox or job rows that several workers share: the seal job of a sealed field reads a page of the rows of
// the entity, in order of its ID, to write the sealed column (ST-SUP-RP10-LC50).
var pagedRowLocks = []string{"internal/generator/postgresadapter/schema_seal.go"}

// claimFaults gives each fault of ST-STD-TXN-R14 in a file of store code, and the count of its claims.
func claimFaults(file storeText) ([]string, int) {
	var faults []string
	claims := 0
	words := sqlWords(file.text, !strings.HasSuffix(file.name, ".sql") && !strings.HasSuffix(file.name, ".sql.tmpl"))
	for i, word := range words {
		end := lockAt(words, i)
		if end < 0 {
			continue
		}
		skipLocked, limitAfter := lockTail(words[end:min(len(words), end+claimReach)])
		if !limitAfter && !limitBefore(words[max(0, i-claimReach):i]) {
			continue
		}
		claims++
		if !skipLocked && !slices.Contains(pagedRowLocks, file.name) {
			faults = append(faults, fmt.Sprintf("%s claims a batch of rows with a row lock and no SKIP LOCKED: %s", file.name, lineOf(file.text, word.at)))
		}
	}
	if !slices.Contains(sessionLockOwners, file.name) {
		for _, match := range sessionLock.FindAllStringIndex(file.text, -1) {
			faults = append(faults, fmt.Sprintf("%s takes a session-scoped advisory lock: %s", file.name, lineOf(file.text, match[0])))
		}
	}
	return faults, claims
}

// tenantSetConfigCall finds each set_config of app.tenant; setConfigLocal reads its third argument.
var (
	tenantSetConfigCall = regexp.MustCompile(`(?i)set_config\s*\(\s*'app\.tenant'\s*,`)
	tenantSet           = regexp.MustCompile(`(?i)\bSET\s+(SESSION\s+|LOCAL\s+)?app\.tenant\b`)
)

// setConfigLocal gives the third argument of the set_config call whose second argument starts at the offset: the
// text after the comma that ends the value at bracket depth zero, outside quotes, up to the close of the call.
func setConfigLocal(text string, offset int) (string, bool) {
	depth, quoted := 0, false
	for i := offset; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return "", false
			}
			depth--
		case c == ',' && depth == 0:
			rest := text[i+1:]
			end := strings.Index(rest, ")")
			if end < 0 {
				return "", false
			}
			return strings.TrimSpace(rest[:end]), true
		}
	}
	return "", false
}

// tenantSetFaults gives each fault of ST-STD-TXN-R02 in the text, and the count of the sets of app.tenant.
func tenantSetFaults(name, text string) ([]string, int) {
	var faults []string
	sets := 0
	for _, match := range tenantSetConfigCall.FindAllStringIndex(text, -1) {
		sets++
		if local, ok := setConfigLocal(text, match[1]); !ok || strings.ToLower(local) != "true" {
			faults = append(faults, fmt.Sprintf("%s sets app.tenant for the session: %s", name, lineOf(text, match[0])))
		}
	}
	for _, match := range tenantSet.FindAllStringIndex(text, -1) {
		faults = append(faults, fmt.Sprintf("%s sets app.tenant with SET: %s", name, lineOf(text, match[0])))
	}
	return faults, sets
}

// A role that STEGO's code creates, with the attributes that the statement gives it: CREATE ROLE or CREATE USER, in
// SQL text, or in a format string whose attributes are an argument (%s), whose value the file sets in one flags
// variable.
var (
	createRole  = regexp.MustCompile(`(?i)CREATE\s+(?:ROLE|USER)\s+\S+\s+([A-Z% ]*)`)
	roleFlags   = regexp.MustCompile(`flags\s*:?=\s*"([A-Z ]+)"`)
	formatFlags = "%S"
	alterRole   = regexp.MustCompile(`(?i)\bALTER\s+(?:ROLE|USER)\s+\S+\s+(?:WITH\s+)?[A-Z ]*\b(SUPERUSER|BYPASSRLS)\b`)
	// A grant of a role, GRANT <roles> TO, with no ON: the member gets the privileges of the role (security review of
	// PR #504, L3).
	grantRole = regexp.MustCompile(`(?is)\bGRANT\s+((?:"?[a-z_][a-z0-9_]*"?\s*,\s*)*"?[a-z_][a-z0-9_]*"?)\s+TO\b`)
)

// powerfulRoles are the roles of PostgreSQL and of hosted PostgreSQL services whose membership gives a superuser,
// a bypass of row-level security or the files and programs of the server.
var powerfulRoles = map[string]bool{"postgres": true, "pg_read_server_files": true, "pg_write_server_files": true,
	"pg_execute_server_program": true, "pg_read_all_data": true, "pg_write_all_data": true, "pg_database_owner": true,
	"rds_superuser": true, "azure_pg_admin": true, "cloudsqlsuperuser": true, "alloydbsuperuser": true}

// roleFaults gives each fault of ST-STD-TXN-R03 in a file of store code, and the count of the roles it creates.
func roleFaults(file storeText) ([]string, int) {
	var faults []string
	var flags []string
	for _, match := range roleFlags.FindAllStringSubmatch(file.text, -1) {
		flags = append(flags, match[1])
	}
	roles := 0
	for _, match := range createRole.FindAllStringSubmatch(file.text, -1) {
		roles++
		attributes := strings.Fields(strings.ToUpper(match[1]))
		if slices.Contains(attributes, formatFlags) {
			if len(flags) != 1 {
				faults = append(faults, fmt.Sprintf("%s creates a role with attributes from an argument, and does not set them in one flags variable", file.name))
				continue
			}
			attributes = append(attributes, strings.Fields(flags[0])...)
		}
		for _, want := range []string{"NOSUPERUSER", "NOBYPASSRLS"} {
			if !slices.Contains(attributes, want) {
				faults = append(faults, fmt.Sprintf("%s creates a role with no %s: %s", file.name, want, strings.TrimSpace(match[0])))
			}
		}
	}
	for _, match := range alterRole.FindAllStringIndex(file.text, -1) {
		faults = append(faults, fmt.Sprintf("%s gives a role SUPERUSER or BYPASSRLS: %s", file.name, lineOf(file.text, match[0])))
	}
	for _, match := range grantRole.FindAllStringSubmatchIndex(file.text, -1) {
		for _, role := range strings.Split(file.text[match[2]:match[3]], ",") {
			if role = strings.ToLower(strings.Trim(strings.TrimSpace(role), `"`)); powerfulRoles[role] {
				faults = append(faults, fmt.Sprintf("%s grants the role %s: %s", file.name, role, lineOf(file.text, match[0])))
			}
		}
	}
	return faults, roles
}

// An audit events table in a grant: the name of the table as the runtime names it, with its schema in any form,
// such as {s}.{events}, stego_audit.audit_event, or the Go joins RecordSchema + "." + EventsTable.
var (
	auditGrant      = regexp.MustCompile(`(?is)GRANT\s+([A-Z0-9_,()"\s]+?)\s+ON\s+(?:TABLE\s+)?([^;]{0,160}?)\s+TO\b`)
	auditEventsName = regexp.MustCompile(`(?i)\{events\}|\baudit_event\b|EventsTable|\bstego_audit_sink\b`)
	// A grant on each table of a schema, which holds the audit table when the schema is an audit schema.
	allTablesIn = regexp.MustCompile(`(?i)\bALL\s+TABLES\s+IN\s+SCHEMA\b`)
	// The comments and the joins of two Go string literals that grantText removes (security review sec-21-14c, L-14c).
	grantBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	grantLineComment  = regexp.MustCompile(`(?m)(^|[ \t])--[^\n]*`)
	grantJoin         = regexp.MustCompile("[`\"][ \t]*\\+[ \t\r\n]*[`\"]")
)

// grantText gives the text of a file of store code for the scan of audit grants: with block comments, -- comments
// that follow a space, and each join of two Go string literals, such as "GRANT INSERT, " + "UPDATE ON", replaced by a
// space, so that a grant across a comment or a join reads as one statement (security review sec-21-14c, L-14c).
func grantText(text string) string {
	text = grantBlockComment.ReplaceAllString(text, " ")
	text = grantLineComment.ReplaceAllString(text, "$1 ")
	return grantJoin.ReplaceAllString(text, " ")
}

// auditTableFiles are the files of the runtime library whose grants on a table are all on an audit table: the
// table of the default sink, stego_audit_sink.entries, which the file names through Go joins (security review of PR
// #504, L3).
var auditTableFiles = []string{runtimeModule + "/audit/auditsink/table.go"}

// auditGrantFaults gives each grant of UPDATE, DELETE, TRUNCATE or ALL on the audit events table in a file of store
// code (review of PR #504, M5), and the count of the grants on it.
func auditGrantFaults(file storeText) ([]string, int) {
	var faults []string
	grants := 0
	text := grantText(file.text)
	for _, match := range auditGrant.FindAllStringSubmatchIndex(text, -1) {
		target := text[match[4]:match[5]]
		onAudit := auditEventsName.MatchString(target) || slices.Contains(auditTableFiles, file.name) && !regexp.MustCompile(`(?i)^(SCHEMA|FUNCTION|SEQUENCE)\b`).MatchString(target)
		if allTablesIn.MatchString(target) {
			onAudit = onAudit || strings.HasPrefix(file.name, runtimeModule+"/audit/") || regexp.MustCompile(`(?i)audit|RecordSchema|\{s\}`).MatchString(target)
		}
		if !onAudit {
			continue
		}
		grants++
		privileges := strings.ToUpper(text[match[2]:match[3]])
		if regexp.MustCompile(`\b(UPDATE|DELETE|TRUNCATE|ALL)\b`).MatchString(privileges) {
			faults = append(faults, fmt.Sprintf("%s grants %s on the audit table: %s", file.name, strings.TrimSpace(privileges), lineOf(text, match[0])))
		}
	}
	return faults, grants
}

// A float in SQL: a column or a cast of type double precision, real, float or float4/float8 (review of PR #504, M6).
var sqlFloat = regexp.MustCompile(`(?i)\b(double\s+precision|real|float[48]?)\b`)

// moneyCode are the folders whose code holds amounts: the money and ledger runtime and the ledger generator.
var moneyCode = []string{runtimeModule + "/money/", runtimeModule + "/ledger/", "internal/generator/ledger/"}

// moneyFaults gives each float in the money code: float32 or float64 in Go, a float type in SQL text, math/big or
// strconv.ParseFloat.
func moneyFaults(file storeText) []string {
	var faults []string
	for _, match := range regexp.MustCompile(`\bfloat(32|64)\b|"math/big"|ParseFloat\(`).FindAllStringIndex(file.text, -1) {
		faults = append(faults, fmt.Sprintf("%s holds an amount as a float: %s", file.name, lineOf(file.text, match[0])))
	}
	for _, match := range regexp.MustCompile("(?s)[`\"][^`\"]*").FindAllStringIndex(file.text, -1) {
		literal := file.text[match[0]:match[1]]
		for _, found := range sqlFloat.FindAllStringIndex(literal, -1) {
			if regexp.MustCompile(`(?i)\b(CREATE|ALTER|SELECT|INSERT|::|CAST|numeric|bigint)\b`).MatchString(literal) {
				faults = append(faults, fmt.Sprintf("%s holds an amount as a float in SQL: %s", file.name, lineOf(file.text, match[0]+found[0])))
			}
		}
	}
	return faults
}

func TestStoreScans(t *testing.T) {
	t.Run("ST-STD-TXN-R02-T02 The scan of the tenant setting tells a transaction-local set from a session set", func(t *testing.T) {
		for text, want := range map[string]int{
			`SELECT pg_catalog.set_config('app.tenant', $1, true)`:                      0,
			`PERFORM set_config('app.tenant', coalesce(saved, ''), true);`:              0,
			`PERFORM set_config('app.tenant', coalesce(saved, ''), false);`:             1,
			`SELECT set_config('app.tenant', $1, false)`:                                1,
			"SELECT set_config('app.tenant',\n  pg_catalog.format('%s', $1),\n  false)": 1,
			`SELECT set_config('app.tenant', 'a,b)', true)`:                             0,
			`SET app.tenant = 'a'`:       1,
			`SET LOCAL app.tenant = 'a'`: 1,
			// Security review of PR #504, L3: a space before the parenthesis and upper case.
			`SELECT set_config ('app.tenant', $1, false)`: 1,
			`SELECT SET_CONFIG('app.tenant', $1, FALSE)`:  1,
			`SELECT SET_CONFIG('app.tenant', $1, TRUE)`:   0,
		} {
			if faults, _ := tenantSetFaults("text", text); len(faults) != want {
				t.Errorf("%q gives %d faults, want %d: %v", text, len(faults), want, faults)
			}
		}
	})
	t.Run("ST-STD-TXN-R14-T02 The scan of claims finds a claim with no SKIP LOCKED across lines and Go string joins", func(t *testing.T) {
		for text, want := range map[string]int{
			"ORDER BY m.sequence LIMIT p_limit FOR UPDATE OF m SKIP LOCKED":       0,
			"ORDER BY m.sequence LIMIT p_limit\n  FOR UPDATE OF m\n  SKIP LOCKED": 0,
			"ORDER BY m.sequence LIMIT p_limit\n  FOR UPDATE OF m;":               1,
			"`... LIMIT 32 ` + \"FOR UPDATE\"":                                    1,
			"WHERE m.id = p_id FOR UPDATE":                                        0,
			"SELECT pg_try_advisory_lock(1)":                                      1,
			// Security review of PR #504, L3: the lock before the limit, FETCH FIRST, the share locks and the shared
			// session lock.
			"ORDER BY m.sequence FOR UPDATE OF m LIMIT 10":                               1,
			"ORDER BY m.sequence FOR UPDATE OF m SKIP LOCKED LIMIT 10":                   0,
			"ORDER BY m.sequence FETCH FIRST 10 ROWS ONLY FOR UPDATE":                    1,
			"ORDER BY m.sequence FETCH FIRST 10 ROWS ONLY FOR NO KEY UPDATE SKIP LOCKED": 0,
			"ORDER BY m.sequence LIMIT 10 FOR SHARE":                                     1,
			"ORDER BY m.sequence LIMIT 10 FOR KEY SHARE SKIP LOCKED":                     0,
			"SELECT pg_advisory_lock_shared(1)":                                          1,
			"SELECT PG_ADVISORY_LOCK (1)":                                                1,
			"SELECT pg_advisory_xact_lock(1)":                                            0,
			// The forms that the regular expression of the scan missed (security review of the store scans, sec-10).
			"SELECT id FROM q ORDER BY id LIMIT 10 OFFSET 5 FOR UPDATE":                      1,
			"SELECT id FROM q ORDER BY id FOR UPDATE NOWAIT LIMIT 10":                        1,
			`SELECT id FROM q m ORDER BY id FOR UPDATE OF "m" LIMIT 10`:                      1,
			`SELECT id FROM q m ORDER BY id LIMIT 10 FOR UPDATE OF "m" SKIP LOCKED`:          0,
			"SELECT id FROM q ORDER BY id LIMIT (n) FOR UPDATE":                              1,
			"SELECT id FROM q ORDER BY id LIMIT @n FOR UPDATE":                               1,
			"SELECT id FROM q ORDER BY id LIMIT %d FOR UPDATE":                               1,
			"`SELECT id FROM q ORDER BY id LIMIT ` + strconv.Itoa(n) + ` FOR UPDATE`":        1,
			"SELECT id FROM q ORDER BY id LIMIT 10 /* one batch */ FOR UPDATE":               1,
			"SELECT id FROM q ORDER BY id LIMIT 10 -- one batch\n  FOR UPDATE":               1,
			"SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE OF stego_outbox.m SKIP LOCKED": 0,
			// A LIMIT of an earlier Go statement or of the arguments of the call is not a claim. The LIMIT of a subquery
			// is: the lock takes the batch of rows that it gives (decision on the security review of PR #14, L3).
			"SELECT id FROM q WHERE id IN (SELECT id FROM r LIMIT 1) FOR UPDATE":                  1,
			"read = from + ` LIMIT $2`\n\tif lock {\n\t\tlock = from + ` FOR NO KEY UPDATE`\n\t}": 0,
			"tx.QueryContext(ctx, `SELECT id FROM q WHERE id = $1 FOR UPDATE`, id, limit)":        0,
			// Security review of PR #14, L3: a trailing limit in any case, a lock across a Go join or a comment, a Go
			// comment after the SQL text, a WITH query and a quoted name with a space.
			"select id from q order by id for update limit 10":                                                              1,
			"SELECT id FROM q ORDER BY id FOR UPDATE limit 10":                                                              1,
			"SELECT id FROM q ORDER BY id FOR UPDATE Limit 10":                                                              1,
			"select id from q order by id for update fetch first 10 rows only":                                              1,
			"`SELECT id FROM q ORDER BY id LIMIT 10 FOR ` + `UPDATE`":                                                       1,
			"`SELECT id FROM q ORDER BY id LIMIT 10 FOR`+\n`UPDATE`":                                                        1,
			"SELECT id FROM q ORDER BY id LIMIT 10 FOR/**/UPDATE":                                                           1,
			"x := `SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE` // SKIP LOCKED\n":                                      1,
			"x := `SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE`\n\t// SKIP LOCKED\n":                                   1,
			"SELECT * FROM q WHERE id IN (SELECT id FROM q ORDER BY id LIMIT 10) FOR UPDATE":                                1,
			"WITH c AS (SELECT id FROM q ORDER BY id LIMIT 10) SELECT * FROM q JOIN c USING (id) FOR UPDATE":                1,
			"WITH c AS (SELECT id FROM q ORDER BY id LIMIT 10) SELECT * FROM q JOIN c USING (id) FOR UPDATE SKIP LOCKED":    0,
			`SELECT id FROM q m ORDER BY id FOR UPDATE OF "my m" LIMIT 10`:                                                  1,
			"SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE SKIP\nLOCKED":                                                 0,
			"rows := q(`SELECT x FROM a LIMIT 1`)\nlock := `SELECT id FROM q WHERE id = $1 FOR UPDATE`":                     0,
			"first(`SELECT x FROM a LIMIT 1`)\nsecond(`SELECT id FROM q WHERE id = $1 FOR UPDATE`)":                         0,
			"read := `SELECT id FROM q LIMIT 1`\nlock := `FOR UPDATE`":                                                      0,
			"x := `SELECT id FROM q WHERE id = $1 FOR UPDATE`\nlimit := 10":                                                 0,
			"SELECT a FROM b LIMIT 1 UNION SELECT id FROM q WHERE id = $1 FOR UPDATE":                                       0,
			"SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE /* SKIP LOCKED */":                                            1,
			"SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE SKIP LOCKED; SELECT id FROM r ORDER BY id LIMIT 5 FOR UPDATE": 1,
			// Security review sec-21-14c, L-14b: a quoted name of the SQL text at the end of a line, or before a comma
			// or a bracket, does not end a raw string literal; an escaped quoted name does not end a double quote
			// literal; and a quote in a // comment or a rune literal opens no literal.
			"`SELECT id FROM q m ORDER BY id FOR UPDATE OF \"m\"\n\tLIMIT 10`":                                           1,
			"`WITH c AS (SELECT id FROM q ORDER BY id LIMIT 10)\n\tSELECT * FROM \"q\"\n\tJOIN c USING (id) FOR UPDATE`": 1,
			"`SELECT * FROM q WHERE id IN (SELECT id FROM q ORDER BY id LIMIT 10) AND (\"k\") = 1 FOR UPDATE`":           1,
			"\"SELECT id FROM q m ORDER BY id FOR UPDATE OF \\\"m\\\"\" +\n\t\" LIMIT 10\"":                              1,
			"tx.QueryContext(ctx, \"SELECT id FROM q WHERE id = $1 FOR UPDATE\", id, limit)":                             0,
			"a := 1 // see `x\nb := `SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE` // SKIP LOCKED\n":                 1,
			"q := '`'\nb := `SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE` // SKIP LOCKED\n":                         1,
			"q := '\\\"'\nb := `SELECT id FROM q ORDER BY id LIMIT 10 FOR UPDATE` // SKIP LOCKED\n":                      1,
			"b := `SELECT id FROM q WHERE note = '5\" disk' ORDER BY id LIMIT 10 FOR UPDATE` // SKIP LOCKED\n":           1,
			"b := \"SELECT id FROM q WHERE note = '5\\\" disk' ORDER BY id LIMIT 10 FOR UPDATE\" // SKIP LOCKED\n":       1,
			// Security review sec-21-14c, L-14c: a lock in a WITH query or a subquery with the LIMIT of the outer SELECT
			// is a claim (decision on the review). A LIMIT of another bracket does not count.
			"WITH c AS (SELECT id FROM q ORDER BY id FOR UPDATE) SELECT id FROM c LIMIT 10":                                             1,
			"SELECT id FROM (SELECT id FROM q ORDER BY id FOR UPDATE) s LIMIT 10":                                                       1,
			"SELECT id FROM (SELECT id FROM (SELECT id FROM q ORDER BY id FOR UPDATE) s) t FETCH FIRST 10 ROWS ONLY":                    1,
			"WITH c AS (SELECT id FROM q ORDER BY id FOR UPDATE SKIP LOCKED) SELECT id FROM c LIMIT 10":                                 0,
			"WITH c AS (SELECT id FROM q WHERE id = $1 FOR UPDATE), d AS (SELECT id FROM r LIMIT 1) SELECT id FROM c JOIN d USING (id)": 0,
			"SELECT id FROM (SELECT id FROM q WHERE id = $1 FOR UPDATE) s":                                                              0,
			"x := `WITH c AS (SELECT id FROM q WHERE id = $1 FOR UPDATE) SELECT id FROM c`\nlimit := 10":                                0,
		} {
			if faults, _ := claimFaults(storeText{name: "text", text: text}); len(faults) != want {
				t.Errorf("%q gives %d faults, want %d: %v", text, len(faults), want, faults)
			}
		}
		// A SQL file has no Go string literal, so a quoted name at the end of a line ends nothing there.
		if faults, _ := claimFaults(storeText{name: "claim.sql", text: "SELECT id FROM q m ORDER BY id FOR UPDATE OF \"m\"\n\tLIMIT 10;"}); len(faults) != 1 {
			t.Errorf("the claim with a quoted name at the end of a line in a SQL file gives %d faults, want 1: %v", len(faults), faults)
		}
	})
	t.Run("ST-STD-TXN-R06-T02 The scan of audit grants finds a write grant on the audit table in any form of its name", func(t *testing.T) {
		for text, want := range map[string]int{
			"GRANT SELECT, INSERT ON {s}.{events} TO {role};":                        0,
			"GRANT SELECT, INSERT, UPDATE ON {s}.{events} TO {role};":                1,
			`"GRANT UPDATE, DELETE ON " + RecordSchema + "." + EventsTable + " TO "`: 1,
			"GRANT ALL ON TABLE stego_audit.audit_event TO app;":                     1,
			"GRANT SELECT, INSERT, UPDATE ON {s}.{heads} TO {role};":                 0,
			// Security review of PR #504, L3: each table of the audit schema, and the table of the default sink.
			"GRANT UPDATE ON ALL TABLES IN SCHEMA stego_audit TO app;": 1,
			"GRANT SELECT ON ALL TABLES IN SCHEMA stego_audit TO app;": 0,
			"GRANT DELETE ON stego_audit_sink.entries TO app;":         1,
			// A column grant (security review of the store scans, sec-10).
			"GRANT UPDATE (status) ON stego_audit.audit_event TO app;":         1,
			"GRANT SELECT (id, at), INSERT ON {s}.{events} TO {role};":         0,
			`GRANT UPDATE ("status") ON TABLE stego_audit.audit_event TO app;`: 1,
			// Security review of PR #14, L3: a grant across lines and tabs, as the runtime writes column grants.
			"GRANT INSERT,\n  UPDATE ON stego_audit.audit_event TO app;":             1,
			"GRANT\tUPDATE,\tDELETE ON stego_audit.audit_event TO app;":              1,
			"GRANT SELECT,\n  INSERT (id,\n\tat) ON stego_audit.audit_event TO app;": 0,
			"GRANT UPDATE (\"a b\") ON stego_audit.audit_event TO app;":              1,
			// Security review sec-21-14c, L-14c: a grant across a comment or a join of two Go string literals.
			"GRANT /* x */ UPDATE ON stego_audit.audit_event TO app;":               1,
			"GRANT UPDATE -- x\n ON stego_audit.audit_event TO app;":                1,
			"`GRANT INSERT, ` + `UPDATE ON stego_audit.audit_event TO app;`":        1,
			"\"GRANT INSERT, \" +\n\t\"DELETE ON stego_audit.audit_event TO app;\"": 1,
			"\"GRANT SELECT, \" +\n\t\"INSERT ON stego_audit.audit_event TO app;\"": 0,
			"GRANT SELECT /* , UPDATE */ ON stego_audit.audit_event TO app;":        0,
		} {
			if faults, _ := auditGrantFaults(storeText{name: "text", text: text}); len(faults) != want {
				t.Errorf("%q gives %d faults, want %d: %v", text, len(faults), want, faults)
			}
		}
	})
	t.Run("ST-STD-TXN-R06-T02 The scan of audit grants finds a write grant on the table of the default sink in its Go joins", func(t *testing.T) {
		for text, want := range map[string]int{
			`"GRANT SELECT, INSERT ON TABLE " + table + " TO " + role,`: 0,
			`"GRANT SELECT, UPDATE ON TABLE " + table + " TO " + role,`: 1,
			`"GRANT USAGE ON SCHEMA " + Schema + " TO " + role,`:        0,
		} {
			if faults, _ := auditGrantFaults(storeText{name: runtimeModule + "/audit/auditsink/table.go", text: text}); len(faults) != want {
				t.Errorf("%q gives %d faults, want %d: %v", text, len(faults), want, faults)
			}
		}
	})
	t.Run("ST-STD-TXN-R12-T02 The scan of money finds a float in Go and in SQL", func(t *testing.T) {
		for text, want := range map[string]int{
			"var rate float64":                                         1,
			"`CREATE TABLE t (amount double precision)`":               1,
			"`CREATE TABLE t (amount bigint NOT NULL, currency text)`": 0,
			"`SELECT amount::real FROM t`":                             1,
			"// the real amount is an integer":                         0,
		} {
			if faults := moneyFaults(storeText{name: "text", text: withoutLineComments(text)}); len(faults) != want {
				t.Errorf("%q gives %d faults, want %d: %v", text, len(faults), want, faults)
			}
		}
	})
	t.Run("ST-STD-TXN-R03-T02 The scan of roles finds a role or a user with SUPERUSER or BYPASSRLS", func(t *testing.T) {
		for text, want := range map[string]int{
			"CREATE ROLE r NOLOGIN NOSUPERUSER NOBYPASSRLS;": 0,
			"CREATE ROLE r NOLOGIN NOSUPERUSER BYPASSRLS;":   1,
			"CREATE USER u SUPERUSER;":                       2,
			"ALTER ROLE r WITH BYPASSRLS;":                   1,
			// Security review of PR #504, L3: a grant of a role with the privileges of a superuser.
			"GRANT postgres TO app;":                   1,
			`GRANT app_owner, "rds_superuser" TO app;`: 1,
			"GRANT app_owner TO app_migrator;":         0,
			"GRANT SELECT ON TABLE postgres TO app;":   0,
		} {
			if faults, _ := roleFaults(storeText{name: "text", text: text}); len(faults) != want {
				t.Errorf("%q gives %d faults, want %d: %v", text, len(faults), want, faults)
			}
		}
	})
}

func TestStoreStandard(t *testing.T) {
	_, tree := readHead(t)
	files := storeTexts(t, tree)

	t.Run("ST-STD-TXN-R02-T01 The tenant setting is set only transaction-local, with set_config and true", func(t *testing.T) {
		var faults []string
		sets := 0
		for _, file := range files {
			found, n := tenantSetFaults(file.name, file.text)
			faults, sets = append(faults, found...), sets+n
		}
		if sets == 0 {
			faults = append(faults, "no file sets app.tenant, so the test reads the wrong files")
		}
		report(t, "ST-STD-TXN-R02", faults)
	})

	t.Run("ST-STD-TXN-R03-T01 Each role or user that STEGO creates is NOBYPASSRLS and NOSUPERUSER", func(t *testing.T) {
		var faults []string
		roles := 0
		for _, file := range files {
			found, n := roleFaults(file)
			faults, roles = append(faults, found...), roles+n
		}
		if roles == 0 {
			faults = append(faults, "no file creates a role, so the test reads the wrong files")
		}
		report(t, "ST-STD-TXN-R03", faults)
	})

	t.Run("ST-STD-TXN-R06-T01 No store code grants UPDATE, DELETE, TRUNCATE or ALL on the audit table, and triggers reject them", func(t *testing.T) {
		var faults []string
		grants := 0
		for _, file := range files {
			found, n := auditGrantFaults(file)
			faults, grants = append(faults, found...), grants+n
		}
		if grants == 0 {
			faults = append(faults, "no file grants a privilege on the audit table, so the test reads the wrong files")
		}
		data, _ := tree.File(auditMigration)
		for _, trigger := range []string{
			`BEFORE UPDATE OR DELETE ON {s}.{events}`,
			`BEFORE UPDATE OR DELETE OR TRUNCATE ON {s}.{events}`,
			`CREATE TRIGGER %I BEFORE UPDATE OR DELETE OR TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.audit_reject_change()`,
			`RAISE EXCEPTION 'the audit table rejects %', TG_OP`,
		} {
			if !strings.Contains(string(data), trigger) {
				faults = append(faults, fmt.Sprintf("%s has no %q", auditMigration, trigger))
			}
		}
		report(t, "ST-STD-TXN-R06", faults)
	})

	t.Run("ST-STD-TXN-R12-T01 No code of the money and ledger runtime or of the ledger generator holds an amount as a float", func(t *testing.T) {
		var faults []string
		read := 0
		for _, file := range files {
			if slices.ContainsFunc(moneyCode, func(folder string) bool { return strings.HasPrefix(file.name, folder) }) {
				read++
				faults = append(faults, moneyFaults(file)...)
			}
		}
		if read == 0 {
			faults = append(faults, "the tree holds no money or ledger code, so the test reads the wrong files")
		}
		report(t, "ST-STD-TXN-R12", faults)
	})

	t.Run("ST-STD-TXN-R14-T01 Each claim of rows in the runtime library and the generators skips locked rows and takes no session lock", func(t *testing.T) {
		var faults []string
		claims := 0
		for _, file := range files {
			found, n := claimFaults(file)
			faults, claims = append(faults, found...), claims+n
		}
		if claims == 0 {
			faults = append(faults, "no file of store code holds a claim of a batch of rows, so the test reads the wrong files")
		}
		report(t, "ST-STD-TXN-R14", faults)
	})
}

// auditMigration is the file of the audit runtime that creates the audit table and its triggers.
const auditMigration = runtimeModule + "/audit/jobs.go"
