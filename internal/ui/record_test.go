// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"testing"

	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

func TestRecordExt(t *testing.T) {
	for _, c := range []struct {
		rec  any
		want string
	}{
		{&httprec.Cassette{}, ".json"},
		{&netdev.Record{}, ".netrec.json"},
		{&sshx.Record{}, ".sshrec.json"},
	} {
		if got := recordExt(c.rec); got != c.want {
			t.Errorf("recordExt(%T) = %q，应为 %q", c.rec, got, c.want)
		}
	}
}
