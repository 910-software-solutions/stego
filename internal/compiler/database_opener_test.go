package compiler

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/internal/gen"
)

func TestDatabaseOpenerValidation(t *testing.T) {
	for _, change := range []func(*gen.Wiring){
		func(w *gen.Wiring) { w.NeedsDB = false },
		func(w *gen.Wiring) { w.DatabaseOpener.Namespace = "../storage" },
		func(w *gen.Wiring) { w.DatabaseOpener.Namespace = "other" },
		func(w *gen.Wiring) { w.DatabaseOpener.Function = "Open();panic(1)" },
		func(w *gen.Wiring) { w.DatabaseOpener.Function = "open" },
	} {
		w := databaseOpenerFixture()
		change(w)
		if files, err := Assemble(AssemblerInput{ModuleName: "example.com/probe", Wirings: []ComponentWiring{{Name: "store", Wiring: w}}}); err == nil || len(files) != 0 {
			t.Fatal("invalid database opener produced output")
		}
	}
	if err := validateDatabaseOpener([]ComponentWiring{{Name: "one", Wiring: databaseOpenerFixture()}, {Name: "two", Wiring: databaseOpenerFixture()}}); err == nil {
		t.Fatal("ambiguous database opener accepted")
	}
}

func databaseOpenerFixture() *gen.Wiring {
	return &gen.Wiring{NeedsDB: true, Imports: []string{"storage"}, Constructors: []string{"storage.NewStore(db)"}, DatabaseOpener: &gen.DatabaseOpenerSpec{Namespace: "storage", Function: "OpenDatabase"}}
}

func TestDatabaseOpenerAssembly(t *testing.T) {
	{
		for _, used := range []bool{false, true} {
			w := databaseOpenerFixture()
			if used {
				w.Routes = []string{`mux.Handle("/",store)`}
			}
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/probe", GoVersion: "1.26.11", Wirings: []ComponentWiring{{Name: "store", Wiring: w}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if f.Path == "main.go" {
					s := string(f.Bytes())
					if strings.Contains(s, "storage.OpenDatabase(dsn)") != used {
						t.Fatal("database opener consumption differs from its resource")
					}
					if used && strings.Contains(s, `"database/sql"`) {
						t.Fatal("custom SQL opener added an unused SQL import")
					}
				}
			}
		}
	}
}

func TestDatabaseOpenerUsesRenamedImport(t *testing.T) {
	input := AssemblerInput{Wirings: []ComponentWiring{{Name: "store", Wiring: databaseOpenerFixture()}}}
	imports := importResult{Renames: map[int]map[string]string{0: {"storage": "storage2"}}}
	if databaseOpenExpression(input, imports) != "storage2.OpenDatabase(dsn)" {
		t.Fatal("opener ignored its import alias")
	}
}

func TestDatabasePoolClosesOnStartupFailure(t *testing.T) {
	t.Parallel()
	testDatabaseStartupFailure(t, false)
}

func TestIndependentDatabasePoolClosesOnStartupFailure(t *testing.T) {
	t.Parallel()
	testDatabaseStartupFailure(t, true)
}

func testDatabaseStartupFailure(t *testing.T, independent bool) {
	w := databaseOpenerFixture()
	w.Imports = []string{"internal/postgres"}
	w.Constructors = []string{"postgres.NewStore(db)"}
	w.DatabaseOpener.Namespace = "internal/postgres"
	w.Routes = []string{`mux.Handle("/",store)`}
	w.GoModRequires = map[string]string{"github.com/jackc/pgx/v5": "v5.11.0"}
	wirings := []ComponentWiring{{Name: "store", Wiring: w}}
	if independent {
		w.Routes = nil
		wirings = append(wirings, ComponentWiring{Name: "reader", Wiring: &gen.Wiring{Imports: []string{"reader"}, Constructors: []string{"reader.NewReader()"}, ConstructorResources: map[int][]gen.Resource{0: {gen.SQLDatabase}}, BackgroundTasks: []int{0}}})
	}
	files, err := Assemble(AssemblerInput{ModuleName: "example.com/pool", GoVersion: "1.26.11", Wirings: wirings})
	if err != nil {
		t.Fatal(err)
	}
	storeSource := `package postgres
import("context";"database/sql";"database/sql/driver";"errors";"net/http";"os";"time")
type Store struct{}
func NewStore(*sql.DB)*Store{return &Store{}}
func(*Store)ServeHTTP(http.ResponseWriter,*http.Request){}
func OpenDatabase(dsn string)(*sql.DB,error){if dsn!="private-database-url"{return nil,errors.New("wrong configuration source")};return sql.OpenDB(connector{}),nil}
type connector struct{}
func(connector)Connect(context.Context)(driver.Conn,error){return connection{},nil}
func(connector)Driver()driver.Driver{return drv{}}
type drv struct{}
func(drv)Open(string)(driver.Conn,error){return connection{},nil}
type connection struct{}
func(connection)Prepare(string)(driver.Stmt,error){return nil,errors.New("unused")}
func(connection)Begin()(driver.Tx,error){return nil,errors.New("unused")}
func(connection)Ping(ctx context.Context)error{
 deadline,ok:=ctx.Deadline()
 if ok&&time.Until(deadline)>0&&time.Until(deadline)<=5*time.Second{os.WriteFile(os.Getenv("POOL_PING_DEADLINE"),[]byte("bounded"),0600)}
 return errors.New("private-ping-failure")
}
func(connection)Close()error{return os.WriteFile(os.Getenv("POOL_CLOSED"),[]byte("closed"),0600)}
`
	files = append(files, gen.File{Path: "internal/postgres/store.go", Content: []byte(storeSource)})
	if independent {
		files = append(files, gen.File{Path: "reader/reader.go", Content: []byte(`package reader
import("context";"database/sql")
type Reader struct{}
func NewReader(*sql.DB)*Reader{return &Reader{}}
func(*Reader)Run(ctx context.Context)error{<-ctx.Done();return ctx.Err()}
`)})
	}
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(project, "service")
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "-race", "-o", binary, "."}} {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("pool assembly: %v %s", err, output)
		}
	}
	for _, source := range []string{"environment", "file"} {
		t.Run(source, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			marker := filepath.Join(t.TempDir(), "closed")
			deadlineMarker := filepath.Join(t.TempDir(), "ping-deadline")
			command := exec.CommandContext(ctx, binary)
			command.Env = append(os.Environ(), "DATABASE_URL=private-database-url", "DATABASE_URL_FILE=", "POOL_CLOSED="+marker, "POOL_PING_DEADLINE="+deadlineMarker, "GORACE=atexit_sleep_ms=0")
			if source == "file" {
				name := filepath.Join(t.TempDir(), "private-database-url")
				if err := os.WriteFile(name, []byte("private-database-url\n"), 0440); err != nil {
					t.Fatal(err)
				}
				command.Env = append(command.Env, "DATABASE_URL=", "DATABASE_URL_FILE="+name)
			}
			output, err := command.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || ctx.Err() != nil {
				t.Fatal("database failure did not stop the process", err)
			}
			if strings.Contains(string(output), "private-") || !strings.Contains(string(output), "database.ping") {
				t.Fatal("database failure record is unsafe or missing")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "closed" {
				t.Fatal("database pool was not closed", err)
			}
			if data, err := os.ReadFile(deadlineMarker); err != nil || string(data) != "bounded" {
				t.Fatal("database startup ping had no deadline", err)
			}
		})
	}
}

func TestDatabaseOpenerWithIndependentConsumer(t *testing.T) {
	{
		t.Run("sql", func(t *testing.T) {
			factory := databaseOpenerFixture()
			factory.Imports = []string{"internal/context", "unused"}
			factory.Constructors = []string{"context.NewStore(db)"}
			factory.DatabaseOpener.Namespace = "internal/context"
			factory.PostDBCalls = []string{"context.Migrate(db)"}
			reader := &gen.Wiring{Imports: []string{"reader"}, Constructors: []string{"reader.NewReader()"}, ConstructorResources: map[int][]gen.Resource{0: {gen.SQLDatabase}}, BackgroundTasks: []int{0}}
			files, err := Assemble(AssemblerInput{ModuleName: "example.com/pool", GoVersion: "1.26.11", Wirings: []ComponentWiring{{Name: "factory", Wiring: factory}, {Name: "reader", Wiring: reader}}})
			if err != nil {
				t.Fatal(err)
			}
			var source string
			for _, file := range files {
				if file.Path == "main.go" {
					source = string(file.Content)
				}
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			alias := ""
			for _, imp := range parsed.Imports {
				imported, _ := strconv.Unquote(imp.Path.Value)
				if imported == "example.com/pool/internal/context" && imp.Name != nil {
					alias = imp.Name.Name
				}
			}
			if alias == "" || alias == "context" || !strings.Contains(source, alias+".OpenDatabase(dsn)") {
				t.Fatal("independent consumer bypassed the declared pool factory or its import alias")
			}
			for _, unexpected := range []string{"example.com/pool/unused", "NewStore(", "Migrate(", `sql.Open("pgx"`} {
				if strings.Contains(source, unexpected) {
					t.Fatal("factory consumed unrelated wiring", unexpected)
				}
			}
		})
	}
}

// TestDatabasePingFaultRecord proves that a start whose database refuses the login names the fault, the object, the
// remedy and the cause in the failure record, as the start of an api names them (walkthrough 25, P3-11).
func TestDatabasePingFaultRecord(t *testing.T) {
	t.Run("ST-SUP-RC02-OC57-T05 A start whose database refuses the ping names the fault database.connection, the object, the remedy and the cause", func(t *testing.T) {
		w := databaseOpenerFixture()
		w.Imports = []string{"internal/postgres"}
		w.Constructors = []string{"postgres.NewStore(db)"}
		w.DatabaseOpener.Namespace = "internal/postgres"
		w.Routes = []string{`mux.Handle("/",store)`}
		files, err := Assemble(AssemblerInput{ModuleName: "example.com/pingfault", GoVersion: "1.26.11", Wirings: []ComponentWiring{{Name: "store", Wiring: w}}})
		if err != nil {
			t.Fatal(err)
		}
		storeSource := `package postgres
import("context";"database/sql";"database/sql/driver";"errors";"net/http")
type Store struct{}
func NewStore(*sql.DB)*Store{return &Store{}}
func(*Store)ServeHTTP(http.ResponseWriter,*http.Request){}
func OpenDatabase(dsn string)(*sql.DB,error){if dsn!="private-database-url"{return nil,errors.New("wrong configuration source")};return sql.OpenDB(connector{}),nil}
type connector struct{}
func(connector)Connect(context.Context)(driver.Conn,error){return connection{},nil}
func(connector)Driver()driver.Driver{return drv{}}
type drv struct{}
func(drv)Open(string)(driver.Conn,error){return connection{},nil}
type connection struct{}
func(connection)Prepare(string)(driver.Stmt,error){return nil,errors.New("unused")}
func(connection)Begin()(driver.Tx,error){return nil,errors.New("unused")}
func(connection)Close()error{return nil}
// refused is the error of a server that refuses the password, as pgconn.PgError gives it.
type refused struct{}
func(refused)Error()string{return "private-password-authentication-failed"}
func(refused)SQLState()string{return "28P01"}
func(connection)Ping(context.Context)error{return refused{}}
`
		files = append(files, gen.File{Path: "internal/postgres/store.go", Content: []byte(storeSource)})
		project := t.TempDir()
		for _, file := range files {
			name := filepath.Join(project, file.Path)
			if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
		binary := filepath.Join(project, "service")
		for _, args := range [][]string{{"mod", "tidy"}, {"build", "-o", binary, "."}} {
			command := exec.Command("go", args...)
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("ping assembly: %v %s", err, output)
			}
		}
		command := exec.Command(binary)
		command.Env = append(os.Environ(), "DATABASE_URL=private-database-url", "DATABASE_URL_FILE=")
		output, err := command.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("a refused ping did not stop the process: %v", err)
		}
		if strings.Contains(string(output), "private-") {
			t.Fatalf("the failure record holds the text of the cause: %s", output)
		}
		record := failureRecordOf(t, output)
		for key, want := range map[string]string{"stage": "database.ping", "fault": "database.connection", "object": "database",
			"cause": "authentication_failed", "sqlstate": "28P01"} {
			if got := record(key); got != want {
				t.Fatalf("the failure record gives %s %q, want %q: %s", key, got, want, output)
			}
		}
		if !strings.Contains(record("remedy"), "DATABASE_URL") {
			t.Fatalf("the failure record gives no remedy that names DATABASE_URL: %s", output)
		}
	})
}

// failureRecordOf gives the reader of the fields of the line service.failed in output: a field at the top level of
// the line, or under attributes with the prefix stego..
func failureRecordOf(t *testing.T, output []byte) func(string) string {
	t.Helper()
	for _, line := range strings.Split(string(output), "\n") {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) != nil || fields["event.name"] != "service.failed" {
			continue
		}
		return func(key string) string {
			if value, ok := fields[key].(string); ok {
				return value
			}
			if attributes, ok := fields["attributes"].(map[string]any); ok {
				if value, ok := attributes["stego."+key].(string); ok {
					return value
				}
			}
			return ""
		}
	}
	t.Fatalf("no line service.failed in %s", output)
	return nil
}
