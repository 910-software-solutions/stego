package delivery

import (
	"strconv"
	"strings"
	"testing"
)

func TestRequirementKind(t *testing.T) {
	t.Run("ST-VO53-C01-T A requirement kind is accepted only when it is one of the nine known kinds", func(t *testing.T) {
		for _, kind := range []string{"secret", "configuration", "backing store", "topic", "identity issuer", "ingress", "egress", "cluster permission", "pod security"} {
			got, err := ParseRequirementKind(kind)
			if err != nil || string(got) != kind {
				t.Errorf("the kind %q is refused: %v", kind, err)
			}
		}
		for _, kind := range []string{"", "database", "Secret", "secret ", "volume"} {
			if _, err := ParseRequirementKind(kind); err == nil {
				t.Errorf("the kind %q is accepted", kind)
			} else if !strings.Contains(err.Error(), strconv.Quote(kind)) {
				t.Errorf("the refusal of %q does not name the kind: %v", kind, err)
			}
		}
	})
}
