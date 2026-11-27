// Package modules imports the modules that the tests of the root module write into a generated module at a version
// other than the one that testdata/modules pins, so that its go.sum pins each one too (ST-STD-LOCAL-R03).
package modules

import (
	_ "github.com/coder/websocket"
	_ "github.com/felixge/httpsnoop"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "k8s.io/api/apps/v1"
	_ "k8s.io/apimachinery/pkg/api/resource"
)
