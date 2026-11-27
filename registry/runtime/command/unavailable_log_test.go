package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/registry/runtime/authz"
)

// unavailableState is an error of the driver with a SQLSTATE and a message, as pgconn.PgError gives them.
type unavailableState struct{ code, message string }

func (e unavailableState) Error() string    { return e.message }
func (e unavailableState) SQLState() string { return e.code }

func TestUnavailableLogClass(t *testing.T) {
	t.Run("ST-SUP-RP10-LC43-T06 The warn line of a database that cannot serve now names the SQLSTATE class only", func(t *testing.T) {
		// Security review of PR #499, L1: class 53 (53100 disk full, 53200 out of memory) is 503, and the warn line
		// gave no class, so a full disk looked like an outage.
		var logs bytes.Buffer
		previous, held := slog.Default(), unavailableLog
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		unavailableLog = authz.NewCauseLog(func() time.Time { return tick })
		t.Cleanup(func() { slog.SetDefault(previous); unavailableLog = held })
		const secret = "MARKED-DATABASE-TEXT"
		for i, c := range []struct {
			err   error
			class string
		}{
			{fmt.Errorf("begin: %w", unavailableState{"53100", "could not extend file: " + secret}), "53"},
			{unavailableState{"53200", "out of memory " + secret}, "53"},
			{unavailableState{"57P01", "terminating connection " + secret}, "57"},
			{unavailableState{"08006", secret}, "08"},
			{unavailableState{"5", secret}, "none"},
			{unavailableState{"a!300", secret}, "xx"},
			{errors.Join(errors.New(secret), syscall.ECONNREFUSED), "none"},
		} {
			logs.Reset()
			logUnavailable(context.Background(), fmt.Sprintf("action-%d", i), "begin", c.err)
			line := logs.String()
			if !strings.Contains(line, "command.database.unavailable") || !strings.Contains(line, "sqlstate_class="+c.class+" ") {
				t.Fatalf("the error %v gave the line %q, want the class %s", c.err, line, c.class)
			}
			if strings.Contains(line, secret) || (len(c.class) == 2 && c.class != "xx" && strings.Contains(line, unavailableCode(c.err))) {
				t.Fatalf("the line %q holds the text or the whole SQLSTATE of the error", line)
			}
		}
	})
}

// unavailableCode gives the whole SQLSTATE of err, or a text no line holds.
func unavailableCode(err error) string {
	var state unavailableState
	if errors.As(err, &state) && len(state.code) > 2 {
		return state.code
	}
	return "no-code"
}
