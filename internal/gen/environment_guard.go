package gen

// EnvironmentGuardSource is the Go source of stegoRefuseEnvironment, which each emitted main calls first (the
// service, a controller process and an RPC process). Libraries that the components use read environment variables
// of their own: the pgx driver reads PG variables for each part that a connection string leaves out, grpc-go reads
// GRPC_ variables, and the Go standard library reads SSL_CERT_FILE and SSL_CERT_DIR, which can change TLS trust, and
// GODEBUG. The guard stops the process when one of those has a value, so that only the declared variables configure
// the service (#344 security review M2). The log settings of grpc-go stay allowed, and so does a GODEBUG with no
// tls or x509 setting, such as panicnil=1: a tls or x509 setting can turn a TLS or certificate check off. The guard names the
// variable, never its value, and uses only the package os, which each main imports.
const EnvironmentGuardSource = `
// stegoRefuseEnvironment stops the process when the environment gives a value to a variable that a library would
// read and that the service does not take. It names the variable, never its value.
func stegoRefuseEnvironment() {
	for _, entry := range os.Environ() {
		name, value := entry, ""
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				name, value = entry[:i], entry[i+1:]
				break
			}
		}
		if value == "" || !stegoRefusedVariable(name) && !(name == "GODEBUG" && stegoWeakensTLS(value)) {
			continue
		}
		for i := 0; i < len(name); i++ {
			if c := name[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				name = "with a name that is not plain"
				break
			}
		}
		os.Stderr.WriteString("the environment variable " + name + " is not taken by this service; give its setting through a declared variable\n")
		os.Exit(2)
	}
}

// stegoRefusedVariable tells whether a library reads the variable and the service does not take it.
func stegoRefusedVariable(name string) bool {
	switch name {
	case "SSL_CERT_FILE", "SSL_CERT_DIR":
		return true
	case "GRPC_GO_LOG_SEVERITY_LEVEL", "GRPC_GO_LOG_VERBOSITY_LEVEL", "GRPC_GO_LOG_FORMATTER":
		return false
	}
	return len(name) >= 2 && name[:2] == "PG" || len(name) >= 5 && name[:5] == "GRPC_"
}

// stegoWeakensTLS tells whether a GODEBUG value holds a tls or an x509 setting.
func stegoWeakensTLS(value string) bool {
	start := 0
	for i := 0; i <= len(value); i++ {
		if i == len(value) || value[i] == ',' {
			key := value[start:i]
			for len(key) > 0 && key[0] == ' ' {
				key = key[1:]
			}
			if len(key) >= 3 && key[:3] == "tls" || len(key) >= 4 && key[:4] == "x509" {
				return true
			}
			start = i + 1
		}
	}
	return false
}
`

// RefusedVariable is stegoRefusedVariable for a test of the release scan: whether the guard refuses a variable by its
// name. GODEBUG is refused only with a tls or x509 setting.
func RefusedVariable(name string) bool {
	switch name {
	case "SSL_CERT_FILE", "SSL_CERT_DIR":
		return true
	case "GRPC_GO_LOG_SEVERITY_LEVEL", "GRPC_GO_LOG_VERBOSITY_LEVEL", "GRPC_GO_LOG_FORMATTER":
		return false
	}
	return len(name) >= 2 && name[:2] == "PG" || len(name) >= 5 && name[:5] == "GRPC_"
}
