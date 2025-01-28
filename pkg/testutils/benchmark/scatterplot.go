// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package benchmark

import (
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type ScatterPlot struct {
	file        *os.File
	timeOffsets []time.Time
	bucketSize  int
	bucketMod   int
	index       int
	startTime   time.Time
	configured  bool
}

var scatterPlotConfig struct {
	path       *string
	bucketSize *int
}

func ScatterPlotFlags() {
	scatterPlotConfig.path = flag.String("scatterplot-file", "", "outputs a scatter plot file")
	scatterPlotConfig.bucketSize = flag.Int("scatterplot-bucket-size", 10, "number of iterations to bucket per point")
}

func NewScatterPlot(b *testing.B) *ScatterPlot {
	if b.N <= 1 || *scatterPlotConfig.path == "" {
		return &ScatterPlot{
			configured: false,
		}
	}
	if *scatterPlotConfig.bucketSize <= 0 {
		b.Fatalf("bucket size must be greater than 0")
	}
	// The output is opened in append mode to allow for multiple runs to be appended to the same file.
	file, err := os.OpenFile(*scatterPlotConfig.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	require.NoError(b, err)
	return &ScatterPlot{
		file:        file,
		timeOffsets: make([]time.Time, b.N/(*scatterPlotConfig.bucketSize)),
		bucketSize:  *scatterPlotConfig.bucketSize,
		bucketMod:   *scatterPlotConfig.bucketSize - 1,
		configured:  true,
	}
}

func (sp *ScatterPlot) Start() {
	if !sp.configured {
		return
	}
	sp.startTime = time.Now()
}

func (sp *ScatterPlot) Stop(b *testing.B) {
	if !sp.configured || sp.index == 0 {
		return
	}
	for i := 0; i < len(sp.timeOffsets); i++ {
		subTime := sp.startTime
		if i > 0 {
			subTime = sp.timeOffsets[i-1]
		}
		_, err := fmt.Fprintf(sp.file, "%d %d\n", i, sp.timeOffsets[i].Sub(subTime).Nanoseconds())
		require.NoError(b, err)
	}
	err := sp.file.Close()
	require.NoError(b, err)
}

func (sp *ScatterPlot) Measure(i int) {
	if !sp.configured {
		return
	}
	if i%sp.bucketSize == sp.bucketMod {
		sp.timeOffsets[sp.index] = time.Now()
		sp.index++
	}
}
