package service

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
)

// The fixed names of the settings of the process environment (ST-SUP-RP14-LC01).
const (
	// DatabaseURLSetting is the connection string of the runtime role, and DatabaseURLFileSetting the absolute path
	// of a file that holds it: one of the two.
	DatabaseURLSetting     = "DATABASE_URL"
	DatabaseURLFileSetting = "DATABASE_URL_FILE"
	// MigrationDatabaseURLSetting is the connection string of the migration job: a login that is the owner role or
	// may SET ROLE to it, and never the runtime role. MigrationDatabaseURLFileSetting is a file that holds it.
	MigrationDatabaseURLSetting     = "STEGO_MIGRATION_DATABASE_URL"
	MigrationDatabaseURLFileSetting = "STEGO_MIGRATION_DATABASE_URL_FILE"
	// IssuerSetting and AudienceSetting are the issuer and the audience that each bearer token must name.
	IssuerSetting   = "STEGO_AUTHN_ISSUER"
	AudienceSetting = "STEGO_AUTHN_AUDIENCE"
	// KeysURLSetting and KeysFileSetting name the key document of the issuer: one of the two, and neither when the
	// declaration states discovery or a public key file.
	KeysURLSetting  = "STEGO_AUTHN_KEYS_URL"
	KeysFileSetting = "STEGO_AUTHN_KEYS_FILE"
	// CAFileSetting is the PEM file of certificate authorities that the verifier adds to the system roots for the
	// discovery document and the key document of the issuer; it never replaces them (ST-SUP-RP13-LC08).
	CAFileSetting = "STEGO_AUTHN_CA_FILE"
	// SpiceDBEndpointSetting is the address of SpiceDB, and SpiceDBKeyFileSetting the file of its preshared key.
	SpiceDBEndpointSetting = "STEGO_SPICEDB_ENDPOINT"
	SpiceDBKeyFileSetting  = "STEGO_SPICEDB_KEY_FILE"
	// SpiceDBInsecureLoopbackSetting set to 1 lets the connection to a loopback SpiceDB address go without TLS, for a
	// local test only; every other address needs verified TLS.
	SpiceDBInsecureLoopbackSetting = "STEGO_SPICEDB_INSECURE_LOOPBACK"
	// SpiceDBCAFileSetting is the PEM file of certificate authorities that the connection to SpiceDB adds to the
	// system roots, for a SpiceDB whose certificate a private authority signs; it never replaces them, and it is
	// optional (ST-SUP-RP14-LC14).
	SpiceDBCAFileSetting = "STEGO_SPICEDB_CA_FILE"
	// CursorKeyFileSetting is the file of the key of the list cursors: at least query.MinCursorKey bytes, the same in
	// every replica.
	CursorKeyFileSetting = "STEGO_QUERY_CURSOR_KEY_FILE"
	// ExtraCursorKeyFileSetting is the file of a second key of the list cursors, of the same form, which opens a
	// cursor and seals none: the new key before every replica has it, or the old key after a rotation
	// (ST-SUP-RP11-LC23). It is optional.
	ExtraCursorKeyFileSetting = "STEGO_QUERY_CURSOR_EXTRA_KEY_FILE"
	// KeyRingFileSetting is the file of the key ring of the secret fields (ST-SUP-RP16-LC02), which the api, the worker
	// and the controller modes read, and the migration job never reads (ST-SUP-RP14-LC12).
	KeyRingFileSetting = "STEGO_SECRET_KEY_RING_FILE"
	// VersionSetting is the version of the release, which each audit row carries.
	VersionSetting = "STEGO_SERVICE_VERSION"
)

// Auth are the settings of the verifier that the declaration states through the settings of its auth provider,
// such as jwt-auth or oidc-auth; an empty one is not stated. A stated setting wins: the environment may only match it.
type Auth struct {
	Issuer, Audience, Header, KeyFile, RoleClaim string
	// Discovery states that the key set comes from the discovery document of the issuer (ST-SUP-RP13-LC21), as
	// oidc-auth states it; the environment then names no key document.
	Discovery bool
	// CAFile is the CA file that the declaration states for the discovery document and the key document.
	CAFile string
	// Algorithms are the declared accepted signing algorithms, and SubjectClaim the declared subject claim; empty
	// gives the verifier's own default, RS256 and sub (ST-SUP-RP13-LC03, LC13).
	Algorithms   []string
	SubjectClaim string
	// Component is the registry component of the auth provider, such as jwt-auth, oidc-auth or rh-sso-auth, which
	// the stage of a refused verifier setting names; empty is jwt-auth.
	Component string
}

// SpiceDB is how the service reaches SpiceDB: its address, its preshared key, whether a loopback address goes
// without TLS, and the roots that verify its certificate.
type SpiceDB struct {
	Endpoint         string
	Key              string
	InsecureLoopback bool
	// Roots are the system roots with the certificates of the CA file added (ST-SUP-RP14-LC14); nil is the system
	// roots alone.
	Roots *x509.CertPool
}

// Settings are the settings of a running service, which ReadSettings reads from the process environment.
type Settings struct {
	// DatabaseURL is the connection string of the runtime role.
	DatabaseURL string
	// Issuer and Audience are the issuer and the audience of the bearer tokens.
	Issuer, Audience string
	// KeysURL and KeysFile name the key document of the issuer, and KeyFile the public key file that the
	// declaration states: one of the three, or none when Discovery holds.
	KeysURL, KeysFile, KeyFile string
	// Discovery states that the key set comes from the discovery document of the issuer, and CAFile is the CA file
	// that adds to the system roots for it and for a key URL.
	Discovery bool
	CAFile    string
	// Algorithms and SubjectClaim are the declared accepted algorithms and subject claim, or empty.
	Algorithms   []string
	SubjectClaim string
	// Header and RoleClaim are the token header and the role claim that the declaration states, or empty.
	Header, RoleClaim string
	// SpiceDB is how the service reaches SpiceDB.
	SpiceDB SpiceDB
	// CursorKey is the key of the list cursors.
	CursorKey []byte
	// ExtraCursorKey is the key that opens a list cursor and seals none, or nil (ST-SUP-RP11-LC23).
	ExtraCursorKey []byte
	// ServiceVersion is the version of the release, or empty.
	ServiceVersion string
	// Ring is the key ring of the secret fields, which ReadKeyRing reads for a service with a readable secret field,
	// or nil (ST-SUP-RP14-LC12).
	Ring *sealed.Ring
}

// MigrationSettings are the settings of the migration job, which ReadMigrationSettings reads.
type MigrationSettings struct {
	// DatabaseURL is the connection string of the migration job.
	DatabaseURL string
	// SpiceDB is how the job reaches SpiceDB to write the schema.
	SpiceDB SpiceDB
}

// SettingError is a missing or malformed setting that ReadSettings or ReadMigrationSettings refused. It names the
// setting and never its value. Variable is the fixed name of the setting at fault, such as DATABASE_URL, and
// Component the registry component that reads it, such as postgres-adapter; StartStage gives the stage
// service.start.setting.<component>.<variable> from them (ST-SUP-RP14-LC05).
type SettingError struct {
	Component, Variable string
	// names are the names that the message gives, such as "DATABASE_URL or DATABASE_URL_FILE", and problem what is
	// wrong with the setting.
	names, problem string
}

func (e *SettingError) Error() string { return "service: the setting " + e.names + " " + e.problem }

// settingError is a missing or malformed setting: it names the setting and never its value. names is one fixed name,
// or two joined by " or ", of which the first is the variable at fault.
func settingError(names, problem string) error {
	variable, _, _ := strings.Cut(names, " or ")
	return &SettingError{Component: settingComponent(variable, ""), Variable: variable, names: names, problem: problem}
}

// settingComponent is the registry component that reads the setting variable (ST-SUP-RP14-LC05): postgres-adapter
// for the connection strings, the auth provider for the verifier settings, spicedb-authz for SpiceDB and query for
// the key of the list cursors. auth is the auth provider that the declaration states, or empty.
func settingComponent(variable, auth string) string {
	switch variable {
	case DatabaseURLSetting, DatabaseURLFileSetting, MigrationDatabaseURLSetting, MigrationDatabaseURLFileSetting,
		WorkerDatabaseURLSetting, WorkerDatabaseURLFileSetting, RelayDatabaseURLSetting, RelayDatabaseURLFileSetting:
		return "postgres-adapter"
	case IssuerSetting, AudienceSetting, KeysURLSetting, KeysFileSetting, CAFileSetting:
		if auth == "" {
			return DefaultAuthComponent
		}
		return auth
	case SpiceDBEndpointSetting, SpiceDBKeyFileSetting, SpiceDBInsecureLoopbackSetting, SpiceDBCAFileSetting:
		return "spicedb-authz"
	case CursorKeyFileSetting, ExtraCursorKeyFileSetting:
		return "query"
	case KeyRingFileSetting:
		return "command"
	}
	return ""
}

// DefaultAuthComponent is the auth provider of the verifier settings when the declaration names none in
// Auth.Component.
const DefaultAuthComponent = "jwt-auth"

// ofComponent gives err with the auth provider of the declaration as the component of a verifier setting at fault.
func ofComponent(err error, stated Auth) error {
	var setting *SettingError
	if errors.As(err, &setting) && stated.Component != "" {
		setting.Component = settingComponent(setting.Variable, stated.Component)
	}
	return err
}

// ReadSettings reads the settings of a running service from the process environment by their fixed names, with the
// verifier settings that the declaration states (ST-SUP-RP14-LC01). It refuses a missing or malformed setting, and a
// setting that differs from the stated one, with a *SettingError that names the setting and the component that reads
// it, and not its value (ST-SUP-RP14-LC05).
func ReadSettings(stated Auth) (Settings, error) {
	s, err := readSettings(stated)
	return s, ofComponent(err, stated)
}

// ReadModeSettings is ReadSettings for a process that runs api, true when it runs the api mode: only the api mode
// serves the list queries, so only it reads the key of the list cursors from CursorKeyFileSetting, and a process with
// no api mode, such as of the worker mode, needs no such file. Such a process gets a new random key, which seals no
// cursor, because it serves no request of the service (core builder 19; ST-DLV-UC07-R06).
func ReadModeSettings(stated Auth, api bool) (Settings, error) {
	s, err := readSettingsOf(stated, api)
	return s, ofComponent(err, stated)
}

// readSettings is ReadSettings before the auth provider of the declaration is set on a refused verifier setting.
func readSettings(stated Auth) (Settings, error) { return readSettingsOf(stated, true) }

// readSettingsOf is readSettings, which reads the key of the list cursors only when api is true.
func readSettingsOf(stated Auth, api bool) (Settings, error) {
	var s Settings
	var err error
	if s.DatabaseURL, err = connectionString(DatabaseURLSetting, DatabaseURLFileSetting); err != nil {
		return Settings{}, err
	}
	if s.Issuer, err = declared(IssuerSetting, stated.Issuer); err != nil {
		return Settings{}, err
	}
	if s.Audience, err = declared(AudienceSetting, stated.Audience); err != nil {
		return Settings{}, err
	}
	s.Header, s.RoleClaim = stated.Header, stated.RoleClaim
	s.Algorithms, s.SubjectClaim = slices.Clone(stated.Algorithms), stated.SubjectClaim
	s.KeysURL, s.KeysFile = os.Getenv(KeysURLSetting), os.Getenv(KeysFileSetting)
	if s.CAFile = os.Getenv(CAFileSetting); stated.CAFile != "" {
		if s.CAFile != "" && s.CAFile != stated.CAFile {
			return Settings{}, settingError(CAFileSetting, "differs from the value that the declaration states, which wins")
		}
		s.CAFile = stated.CAFile
	}
	switch {
	case stated.Discovery && stated.KeyFile != "":
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "has no source: the declaration states both discovery and a public key file")
	case stated.Discovery && (s.KeysURL != "" || s.KeysFile != ""):
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "must not be set: the declaration states discovery through the issuer")
	case stated.Discovery:
		s.Discovery = true
	case stated.KeyFile != "" && (s.KeysURL != "" || s.KeysFile != ""):
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "must not be set: the declaration states a public key file")
	case stated.KeyFile != "":
		s.KeyFile = stated.KeyFile
	case (s.KeysURL == "") == (s.KeysFile == ""):
		return Settings{}, settingError(KeysURLSetting+" or "+KeysFileSetting, "must be set, one of the two")
	}
	if s.CAFile != "" && s.KeysURL == "" && !s.Discovery {
		return Settings{}, settingError(CAFileSetting, "is only for a key URL or discovery through the issuer")
	}
	if s.SpiceDB, err = readSpiceDB(); err != nil {
		return Settings{}, err
	}
	if !api {
		s.CursorKey = make([]byte, query.MinCursorKey)
		if _, err := rand.Read(s.CursorKey); err != nil {
			return Settings{}, err
		}
	} else if s.CursorKey, err = cursorKey(); err != nil {
		return Settings{}, err
	} else if s.ExtraCursorKey, err = extraCursorKey(s.CursorKey); err != nil {
		return Settings{}, err
	}
	s.ServiceVersion = os.Getenv(VersionSetting)
	return s, nil
}

// ReadMigrationSettings reads the settings of the migration job from the process environment by their fixed names
// (ST-SUP-RP14-LC01): the connection string of the migration job and SpiceDB.
func ReadMigrationSettings() (MigrationSettings, error) {
	url, err := connectionString(MigrationDatabaseURLSetting, MigrationDatabaseURLFileSetting)
	if err != nil {
		return MigrationSettings{}, err
	}
	spice, err := readSpiceDB()
	if err != nil {
		return MigrationSettings{}, err
	}
	return MigrationSettings{DatabaseURL: url, SpiceDB: spice}, nil
}

// readSpiceDB reads the address of SpiceDB, its preshared key from the key file, with the checks of settingFile,
// the loopback test mode, and the CA file of spiceDBRoots.
func readSpiceDB() (SpiceDB, error) {
	var s SpiceDB
	var err error
	if s.Endpoint, err = required(SpiceDBEndpointSetting); err != nil {
		return SpiceDB{}, err
	}
	name, err := required(SpiceDBKeyFileSetting)
	if err != nil {
		return SpiceDB{}, err
	}
	key, err := settingFile(SpiceDBKeyFileSetting, name)
	if err != nil {
		return SpiceDB{}, err
	}
	if s.Key = strings.TrimSpace(string(key)); s.Key == "" {
		return SpiceDB{}, settingError(SpiceDBKeyFileSetting, "names an empty key")
	}
	switch os.Getenv(SpiceDBInsecureLoopbackSetting) {
	case "", "0":
	case "1":
		host, _, err := net.SplitHostPort(s.Endpoint)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return SpiceDB{}, settingError(SpiceDBInsecureLoopbackSetting, "allows no TLS only for a loopback address of "+SpiceDBEndpointSetting)
		}
		s.InsecureLoopback = true
	default:
		return SpiceDB{}, settingError(SpiceDBInsecureLoopbackSetting, "must be 0 or 1")
	}
	if s.Roots, err = spiceDBRoots(s.InsecureLoopback, x509.SystemCertPool); err != nil {
		return SpiceDB{}, err
	}
	return s, nil
}

// spiceDBRoots reads the CA file of SpiceDBCAFileSetting, with the checks of settingFile, and gives the system roots
// with its certificates added, or nil when the setting has no value. It refuses a file that holds no PEM certificate,
// and a CA file together with the loopback test mode, which has no TLS to verify (ST-SUP-RP14-LC14).
func spiceDBRoots(insecureLoopback bool, system func() (*x509.CertPool, error)) (*x509.CertPool, error) {
	name := os.Getenv(SpiceDBCAFileSetting)
	if name == "" {
		return nil, nil
	}
	if insecureLoopback {
		return nil, settingError(SpiceDBCAFileSetting, "takes no value when "+SpiceDBInsecureLoopbackSetting+" is 1")
	}
	data, err := settingFile(SpiceDBCAFileSetting, name)
	if err != nil {
		return nil, err
	}
	roots, err := system()
	if err != nil || roots == nil {
		return nil, settingError(SpiceDBCAFileSetting, "needs the system roots, which are missing")
	}
	if roots = roots.Clone(); !roots.AppendCertsFromPEM(data) {
		return nil, settingError(SpiceDBCAFileSetting, "names a file that holds no PEM certificate")
	}
	return roots, nil
}

// ReadKeyRing reads the key ring of the secret fields from the file of KeyRingFileSetting, with the checks of
// settingFile and of sealed.ParseRing. The generated code of a service with a readable secret field calls it for each
// process that runs the api, the worker or the controller mode, and the migration job never calls it. A missing or
// malformed setting is a *SettingError of the component command that names the variable, and never a key
// (ST-SUP-RP14-LC12).
func ReadKeyRing() (*sealed.Ring, error) {
	name, err := required(KeyRingFileSetting)
	if err != nil {
		return nil, err
	}
	data, err := settingFile(KeyRingFileSetting, name)
	if err != nil {
		return nil, err
	}
	ring, err := sealed.ParseRing(data)
	clear(data)
	if err != nil {
		// The error of the ring names a line number and never a key.
		return nil, settingError(KeyRingFileSetting, "does not read as a key ring: "+strings.TrimPrefix(err.Error(), sealed.ErrRing.Error()+": "))
	}
	return ring, nil
}

// cursorKey reads the key of the list cursors from the file of CursorKeyFileSetting, with the checks of
// settingFile.
func cursorKey() ([]byte, error) {
	name, err := required(CursorKeyFileSetting)
	if err != nil {
		return nil, err
	}
	key, err := settingFile(CursorKeyFileSetting, name)
	if err != nil {
		return nil, err
	}
	if len(key) < query.MinCursorKey {
		return nil, settingError(CursorKeyFileSetting, fmt.Sprintf("names a key of fewer than %d bytes", query.MinCursorKey))
	}
	return key, nil
}

// extraCursorKey reads the key of ExtraCursorKeyFileSetting, when it is set, with the checks of cursorKey; it refuses
// a key that is the key of CursorKeyFileSetting (ST-SUP-RP11-LC23).
func extraCursorKey(key []byte) ([]byte, error) {
	name := os.Getenv(ExtraCursorKeyFileSetting)
	if name == "" {
		return nil, nil
	}
	extra, err := settingFile(ExtraCursorKeyFileSetting, name)
	if err != nil {
		return nil, err
	}
	switch {
	case len(extra) < query.MinCursorKey:
		return nil, settingError(ExtraCursorKeyFileSetting, fmt.Sprintf("names a key of fewer than %d bytes", query.MinCursorKey))
	case bytes.Equal(extra, key):
		return nil, settingError(ExtraCursorKeyFileSetting, "names the key of "+CursorKeyFileSetting+"; give the other key of the rotation")
	}
	return extra, nil
}

// declared gives the value that the declaration states, or the setting when it states none. A setting that is set
// to another value than the stated one is refused: the declaration wins.
func declared(name, stated string) (string, error) {
	value := os.Getenv(name)
	if stated == "" {
		if value == "" {
			return "", settingError(name, "is not set, and the declaration states no value for it")
		}
		return value, nil
	}
	if value != "" && value != stated {
		return "", settingError(name, "differs from the value that the declaration states, which wins")
	}
	return stated, nil
}

// required gives the value of the setting name, or an error that names it when it is empty.
func required(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", settingError(name, "is not set")
	}
	return value, nil
}

// errNoConnectionFile is the refusal of a connection string file that is not a safe regular file.
var errNoConnectionFile = errors.New("names no readable connection string file")

// errNoSettingFile is the refusal of a file of a setting that is not a safe regular file.
var errNoSettingFile = errors.New("names no safe readable file: an absolute path of a regular file of at most 64 KiB that no group or other may write")

// maxSettingFile is the largest file of a setting, in bytes.
const maxSettingFile = 65536

// settingFile reads the file at path that the setting name names, with the checks of a connection string file: an
// absolute path of a regular file of at most 64 KiB, which it opens without waiting on a FIFO, that has no execute
// bit for its owner and that no group or other may write or execute. A refusal names the setting and not the path.
func settingFile(name, path string) ([]byte, error) {
	invalid := settingError(name, errNoSettingFile.Error())
	if !filepath.IsAbs(path) {
		return nil, invalid
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, invalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSettingFile || info.Mode().Perm()&0o137 != 0 {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSettingFile+1))
	if err != nil || len(data) > maxSettingFile {
		return nil, invalid
	}
	return data, nil
}

// connectionString gives the connection string of the setting name, or of the file that the setting fileName
// names: one of the two. The file passes the checks of settingFile and holds one line.
func connectionString(name, fileName string) (string, error) {
	value, file := os.Getenv(name), os.Getenv(fileName)
	if (value == "") == (file == "") {
		return "", settingError(name+" or "+fileName, "must be set, exactly one of the two")
	}
	if file == "" {
		return value, nil
	}
	invalid := settingError(fileName, errNoConnectionFile.Error())
	data, err := settingFile(fileName, file)
	if err != nil {
		return "", invalid
	}
	value = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", invalid
	}
	return value, nil
}
