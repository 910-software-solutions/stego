package manifest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// relaxManifest is stepManifest with three more columns of books: the enum mood, the character varying(8) size and
// the optional enum kind, with the CHECKs of their values.
func relaxManifest() Manifest {
	m := stepManifest()
	books := &m.Tables[1]
	books.Columns = append(slices.Clone(books.Columns), Column{Name: "mood", Type: "text", NotNull: true},
		Column{Name: "size", Type: "character varying(8)", NotNull: true}, Column{Name: "kind", Type: "text"})
	books.Constraints = append(slices.Clone(books.Constraints),
		Constraint{Name: "books_mood_check", Kind: KindCheck, Definition: "CHECK ((mood = ANY (ARRAY['calm'::text, 'busy'::text])))"},
		Constraint{Name: "books_kind_check", Kind: KindCheck, Definition: "CHECK ((kind = ANY (ARRAY['a'::text, 'b'::text])))"})
	return m
}

// relaxBase adds the columns of relaxManifest to books.
const relaxBase = "ALTER TABLE public.books ADD COLUMN mood text NOT NULL, ADD COLUMN size character varying(8) NOT NULL, ADD COLUMN kind text, " +
	"ADD CONSTRAINT books_mood_check CHECK (mood IN ('calm', 'busy')), ADD CONSTRAINT books_kind_check CHECK (kind IN ('a', 'b'));\n"

// mark gives the comment of a relax step on the column of books, with the lines of its relaxed form, as
// postgresadapter.RelaxComment writes it.
func mark(column string, lines ...string) string {
	comment := strings.Join(append([]string{RelaxMarker + " step_0001_books-" + column + "-relax_expand"}, lines...), "\n")
	return fmt.Sprintf("COMMENT ON COLUMN public.books.%s IS '%s';\n", column, strings.ReplaceAll(comment, "'", "''"))
}

// The relaxed forms of the four steps.
var (
	titleForm = []string{"type character varying(300)"}
	moodForm  = []string{"check CHECK ((mood = ANY (ARRAY['calm'::text, 'busy'::text, 'sleepy'::text])))"}
	sizeForm  = []string{"nullable", "default 'm'::character varying(8)"}
	kindForm  = []string{"check none"}
)

// The states of the relax steps, as the generator writes them (postgresadapter.relaxSteps): the step's own
// migration, its validate migration and its finish migration.
const (
	titleExpand = "ALTER TABLE public.books ADD CONSTRAINT books_title_relax CHECK (char_length(title) >= 1) NOT VALID, DROP CONSTRAINT books_title_check, ALTER COLUMN title TYPE character varying(300);\n"
	titleValid  = "ALTER TABLE public.books VALIDATE CONSTRAINT books_title_relax;\n"
	titleFinish = "ALTER TABLE public.books RENAME CONSTRAINT books_title_relax TO books_title_check;\n"
	moodExpand  = "ALTER TABLE public.books ADD CONSTRAINT books_mood_relax CHECK (mood IN ('calm', 'busy', 'sleepy')) NOT VALID;\n"
	moodValid   = "ALTER TABLE public.books VALIDATE CONSTRAINT books_mood_relax;\n"
	moodFinish  = "ALTER TABLE public.books DROP CONSTRAINT books_mood_check;\nALTER TABLE public.books RENAME CONSTRAINT books_mood_relax TO books_mood_check;\n"
	sizeExpand  = "ALTER TABLE public.books ALTER COLUMN size DROP NOT NULL, ALTER COLUMN size SET DEFAULT 'm';\n"
	kindExpand  = "ALTER TABLE public.books DROP CONSTRAINT books_kind_check;\n"
)

func TestRelaxForms(t *testing.T) {
	m := relaxManifest()
	title, mood, size, kind := mark("title", titleForm...), mark("mood", moodForm...), mark("size", sizeForm...), mark("kind", kindForm...)

	t.Run("ST-SUP-RP15-LC04-T01 Check accepts each state of each relax step on a marked column", func(t *testing.T) {
		for _, c := range []struct{ name, setup string }{
			{"the columns before any step", relaxBase},
			{"a longer max_length: the step's own migration", relaxBase + title + titleExpand},
			{"a longer max_length: validated", relaxBase + title + titleExpand + titleValid},
			{"a longer max_length: finished", relaxBase + title + titleExpand + titleValid + titleFinish},
			{"a new enum value: the step's own migration", relaxBase + mood + moodExpand},
			{"a new enum value: validated", relaxBase + mood + moodExpand + moodValid},
			{"a new enum value: finished", relaxBase + mood + moodExpand + moodValid + moodFinish},
			{"a required field made optional with a new default", relaxBase + size + sizeExpand},
			{"an enum made a string in place", relaxBase + kind + kindExpand},
			{"a marker alone", relaxBase + title + mood + size + kind},
			{"every step finished", relaxBase + title + titleExpand + titleValid + titleFinish + mood + moodExpand + moodValid + moodFinish + size + sizeExpand + kind + kindExpand},
		} {
			stepCase(t, c.name, c.setup, m, false)
		}
	})

	t.Run("ST-SUP-RP15-LC04-T02 Check refuses a relaxed form on a column with no marker, and a narrower or other form on a marked one", func(t *testing.T) {
		for _, c := range []struct{ name, setup string }{
			// Each finished step with no marker: a column changed by hand.
			{"a longer max_length with no marker", relaxBase + titleExpand + titleValid + titleFinish},
			{"a new enum value with no marker", relaxBase + moodExpand + moodValid + moodFinish},
			{"the new enum CHECK beside the old with no marker", relaxBase + moodExpand},
			{"an optional field with no marker", relaxBase + "ALTER TABLE public.books ALTER COLUMN size DROP NOT NULL;\n"},
			{"a new default with no marker", relaxBase + "ALTER TABLE public.books ALTER COLUMN size SET DEFAULT 'm';\n"},
			{"an enum CHECK dropped with no marker", relaxBase + kindExpand},
			{"the marker on another column", relaxBase + mood + titleExpand + titleValid + titleFinish},
			{"a wrong marker text", relaxBase + "COMMENT ON COLUMN public.books.title IS 'stego relaxed';\n" + titleExpand + titleValid + titleFinish},
			// A marked column in a form that is no relax of the manifest's.
			// A marked column in a form that its relax does not state (security review of PR #334, M1).
			{"a longer character varying than the relax states", relaxBase + title + "ALTER TABLE public.books DROP CONSTRAINT books_title_check, ALTER COLUMN title TYPE character varying(1000000);\nALTER TABLE public.books ADD CONSTRAINT books_title_check CHECK (char_length(title) >= 1);\n"},
			{"NOT NULL dropped on a column whose relax states no nullable", relaxBase + mood + moodExpand + moodValid + moodFinish + "ALTER TABLE public.books ALTER COLUMN mood DROP NOT NULL;\n"},
			{"a default on a column whose relax states none", relaxBase + title + titleExpand + titleValid + titleFinish + "ALTER TABLE public.books ALTER COLUMN title SET DEFAULT 'x';\n"},
			{"another default than the relax states", relaxBase + size + "ALTER TABLE public.books ALTER COLUMN size DROP NOT NULL, ALTER COLUMN size SET DEFAULT 'l';\n"},
			{"a wider enum CHECK than the relax states", relaxBase + mood + "ALTER TABLE public.books DROP CONSTRAINT books_mood_check, ADD CONSTRAINT books_mood_check CHECK (mood IN ('calm', 'busy', 'sleepy', 'angry'));\n"},
			{"the enum CHECK dropped on a column whose relax states a CHECK", relaxBase + mood + "ALTER TABLE public.books DROP CONSTRAINT books_mood_check;\n"},
			{"the enum CHECK dropped on a column whose relax changes no CHECK", relaxBase + mark("kind", "nullable") + kindExpand},
			{"a type on a column whose relax states none", relaxBase + mark("size", "nullable") + "ALTER TABLE public.books ALTER COLUMN size TYPE character varying(9);\n"},
			{"a comment that names no step migration", relaxBase + "COMMENT ON COLUMN public.books.size IS 'stego relax by hand\nnullable';\n" + "ALTER TABLE public.books ALTER COLUMN size DROP NOT NULL;\n"},
			{"a comment with a line that is no relaxed form", relaxBase + mark("size", "nullable", "unique") + "ALTER TABLE public.books ALTER COLUMN size DROP NOT NULL;\n"},
			{"a comment whose relaxed type is shorter than the manifest's", relaxBase + mark("size", "type character varying(4)") + "ALTER TABLE public.books ALTER COLUMN size TYPE character varying(4);\n"},
			{"a shorter character varying", relaxBase + title + "ALTER TABLE public.books ALTER COLUMN title TYPE character varying(4) USING substr(title, 1, 4);\n"},
			{"text in place of character varying", relaxBase + title + "ALTER TABLE public.books DROP CONSTRAINT books_title_check, ALTER COLUMN title TYPE text;\nALTER TABLE public.books ADD CONSTRAINT books_title_check CHECK (char_length(title) >= 1);\n"},
			{"the length CHECK dropped with no relax CHECK", relaxBase + title + "ALTER TABLE public.books DROP CONSTRAINT books_title_check;\n"},
			{"the length CHECK dropped beside a relax CHECK that is not the manifest's", relaxBase + title + "ALTER TABLE public.books ADD CONSTRAINT books_title_relax CHECK (char_length(title) >= 0) NOT VALID, DROP CONSTRAINT books_title_check;\n"},
			{"a wider length CHECK", relaxBase + title + "ALTER TABLE public.books DROP CONSTRAINT books_title_check, ADD CONSTRAINT books_title_check CHECK (char_length(title) >= 0);\n"},
			{"a relax CHECK of the length that is not the manifest's", relaxBase + title + "ALTER TABLE public.books ADD CONSTRAINT books_title_relax CHECK (char_length(title) >= 0) NOT VALID;\n"},
			{"an enum value taken away", relaxBase + mood + "ALTER TABLE public.books DROP CONSTRAINT books_mood_check, ADD CONSTRAINT books_mood_check CHECK (mood IN ('calm', 'sleepy'));\n"},
			{"a relax CHECK that takes a value away", relaxBase + mood + "ALTER TABLE public.books ADD CONSTRAINT books_mood_relax CHECK (mood IN ('calm', 'sleepy')) NOT VALID;\n"},
			{"a relax CHECK with an expression among its values", relaxBase + mood + "ALTER TABLE public.books ADD CONSTRAINT books_mood_relax CHECK (mood = ANY (ARRAY['calm', 'busy', lower('Sleepy')])) NOT VALID;\n"},
			{"a relax CHECK with one more expression", relaxBase + mood + "ALTER TABLE public.books ADD CONSTRAINT books_mood_relax CHECK (mood IN ('calm', 'busy', 'sleepy') AND length(mood) < 9) NOT VALID;\n"},
			{"a relax CHECK of another column under the name", relaxBase + mood + "ALTER TABLE public.books ADD CONSTRAINT books_mood_relax CHECK (kind IN ('a', 'b', 'c')) NOT VALID;\n"},
			{"the wider enum CHECK not validated under its own name", relaxBase + mood + moodExpand + moodFinish},
			{"the relax CHECK of an unmarked column", relaxBase + mood + "ALTER TABLE public.books ADD CONSTRAINT books_kind_relax CHECK (kind IN ('a', 'b', 'c')) NOT VALID;\n"},
			{"a default that is no constant", relaxBase + size + "ALTER TABLE public.books ALTER COLUMN size SET DEFAULT pg_catalog.md5('x');\n"},
			{"a constant default on a marked column of the primary key", relaxBase + mark("id", "default '00000000-0000-0000-0000-000000000000'::uuid") + "ALTER TABLE public.books ALTER COLUMN id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid;\n"},
		} {
			stepCase(t, c.name, c.setup, m, true)
		}
		// A fixed column of the table, such as version, keeps its form also with the marker.
		versioned := relaxManifest()
		versioned.Tables[1].Columns = append(versioned.Tables[1].Columns, Column{Name: "version", Type: "bigint", NotNull: true})
		stepCase(t, "a constant default on a marked fixed column", relaxBase+"ALTER TABLE public.books ADD COLUMN version bigint NOT NULL;\n"+mark("version", "default 1")+"ALTER TABLE public.books ALTER COLUMN version SET DEFAULT 1;\n", versioned, true)
		stepCase(t, "the same column unmarked and with no default (control)", relaxBase+"ALTER TABLE public.books ADD COLUMN version bigint NOT NULL;\n", versioned, false)
	})
}
