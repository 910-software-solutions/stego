package types

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// VariableType is the kind of value that an environment variable holds (ST-VO63). It sets how a component reads the
// value and how a check of an environment file compares it, so both Supplying (the registry) and Delivering (the
// environment check) use the checks of this file.
type VariableType string

// VariableTypes are the known types of ST-VO63-C01.
var VariableTypes = []VariableType{"string", "integer", "number", "duration", "boolean", "url", "path", "address", "list"}

// AllowedValues are the values that a variable accepts: a list of words, or an inclusive range of an integer, a number
// or a duration. The zero value allows each value of the type.
type AllowedValues struct {
	Values   []string
	Min, Max *string
}

// maxVariableValue bounds a value of a variable.
const maxVariableValue = 512

// CheckVariableValue tells whether a value is of the variable's type and inside its allowed values, and why not.
// It never puts the value in its message, because the caller may hold a value that it must not show.
func CheckVariableValue(kind VariableType, allowed AllowedValues, value string) error {
	if len(value) > maxVariableValue || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("the value is not one line of at most %d characters", maxVariableValue)
	}
	if err := CheckVariableType(kind, value); err != nil {
		return err
	}
	if len(allowed.Values) > 0 {
		items := []string{value}
		if kind == "list" {
			items = strings.Split(value, ",")
		}
		for _, item := range items {
			if !slices.Contains(allowed.Values, strings.TrimSpace(item)) {
				return fmt.Errorf("the value is not one of the allowed values %s", strings.Join(allowed.Values, ", "))
			}
		}
		return nil
	}
	number, _ := NumericValue(kind, value)
	if allowed.Min != nil {
		if minimum, _ := NumericValue(kind, *allowed.Min); number < minimum {
			return fmt.Errorf("the value is less than %s", *allowed.Min)
		}
	}
	if allowed.Max != nil {
		if maximum, _ := NumericValue(kind, *allowed.Max); number > maximum {
			return fmt.Errorf("the value is more than %s", *allowed.Max)
		}
	}
	return nil
}

// NumericValue gives an integer, a number or a duration as a number, to compare it with a range.
func NumericValue(kind VariableType, value string) (float64, error) {
	switch kind {
	case "integer":
		n, err := strconv.ParseInt(value, 10, 64)
		return float64(n), err
	case "number":
		return strconv.ParseFloat(value, 64)
	case "duration":
		d, err := time.ParseDuration(value)
		return float64(d), err
	}
	return 0, fmt.Errorf("a %s has no range", kind)
}

// CheckVariableType tells whether a value is of a variable type.
func CheckVariableType(kind VariableType, value string) error {
	switch kind {
	case "string":
		return nil
	case "integer":
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Errorf("the value is not an integer")
		}
	case "number":
		if n, err := strconv.ParseFloat(value, 64); err != nil || n != n {
			return fmt.Errorf("the value is not a number")
		}
	case "duration":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("the value is not a duration such as 30s or 5m")
		}
	case "boolean":
		if !slices.Contains([]string{"true", "false", "0", "1"}, value) {
			return fmt.Errorf("the value is not true, false, 0 or 1")
		}
	case "url":
		u, err := url.Parse(value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("the value is not an absolute URL")
		}
	case "path":
		if !path.IsAbs(value) {
			return fmt.Errorf("the value is not an absolute file path")
		}
	case "address":
		host, port, err := net.SplitHostPort(value)
		number, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || number < 1 || number > 65535 || strings.ContainsAny(host, " /") {
			return fmt.Errorf("the value is not an address of a host and a port")
		}
	case "list":
		for _, item := range strings.Split(value, ",") {
			if strings.TrimSpace(item) == "" {
				return fmt.Errorf("the value is not a list of comma-separated items")
			}
		}
	default:
		return fmt.Errorf("the type %q is not known", kind)
	}
	return nil
}
