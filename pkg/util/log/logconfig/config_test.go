// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package logconfig

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/kr/pretty"
	"gopkg.in/yaml.v2"
)

func TestConfig(t *testing.T) {
	datadriven.RunTest(t, "testdata/yaml", func(t *testing.T, d *datadriven.TestData) string {
		var c Config
		if err := yaml.UnmarshalStrict([]byte(d.Input), &c); err != nil {
			return fmt.Sprintf("ERROR: %v\n", err)
		}
		t.Logf("%# v", pretty.Formatter(c))
		var buf bytes.Buffer
		b, err := yaml.Marshal(&c)
		if err != nil {
			fmt.Fprintf(&buf, "ERROR: %v\n", err)
		} else {
			fmt.Fprintf(&buf, "%s", string(b))
		}
		return buf.String()
	})
}
