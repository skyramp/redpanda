// Copyright 2021 Vectorized, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

//go:build linux
// +build linux

package debug

import (
	"io/ioutil"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLimitedWriter(t *testing.T) {
	const block = 4096
	tests := []struct {
		name          string
		limit         int
		blocksToWrite int
	}{{
		name:          "it should write everything if the limit is larger than the total bytes",
		limit:         3 * block,
		blocksToWrite: 2,
	}, {
		name:          "it should write up to the limit if the total bytes are larger than the limit",
		limit:         block,
		blocksToWrite: 2,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(st *testing.T) {
			lim := &limitedWriter{
				w:          ioutil.Discard,
				limitBytes: tt.limit,
			}
			var writeErr error
			remaining := tt.blocksToWrite
			written := 0
			for remaining > 0 {
				bs := make([]byte, int(block))

				var n int
				n, writeErr = lim.Write(bs)
				written += n
				if writeErr != nil {
					break
				}
				remaining--
			}
			var expected int
			totalBytes := int(tt.blocksToWrite * block)
			if totalBytes > tt.limit {
				require.EqualError(st, writeErr, "output size limit reached")
				expected = tt.limit
			} else if totalBytes <= tt.limit {
				require.NoError(st, writeErr)
				expected = totalBytes
			}
			require.Equal(st, expected, written)
		})
	}
}
