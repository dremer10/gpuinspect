package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestGrepLines(t *testing.T) {
	vvv := strings.Join([]string{
		"be:02.0 PCI bridge: Broadcom / LSI PEX890xx",
		"\tLnkCap:\tPort #2, Speed 32GT/s, Width x4, ASPM L0s L1",
		"\tLnkCap2: Supported Link Speeds: 2.5-32GT/s",
		"\tLnkSta:\tSpeed 16GT/s, Width x1",
		"\tLnkSta2: Current De-emphasis Level: -6dB",
		"\tSlot #136, PowerLimit 25W",
		"\tLnkCtl2: Target Link Speed: 32GT/s",
	}, "\n")

	got := grepLines(vvv, bridgeLinkRe)
	want := []string{
		"LnkCap:\tPort #2, Speed 32GT/s, Width x4, ASPM L0s L1",
		"LnkSta:\tSpeed 16GT/s, Width x1",
		"Slot #136, PowerLimit 25W",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bridgeLinkRe: got %q want %q", got, want)
	}

	got = grepLines(vvv, endpointLinkRe)
	want = []string{
		"LnkCap:\tPort #2, Speed 32GT/s, Width x4, ASPM L0s L1",
		"LnkSta:\tSpeed 16GT/s, Width x1",
		"LnkCtl2: Target Link Speed: 32GT/s",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpointLinkRe: got %q want %q", got, want)
	}
}

func TestNonEmptyLines(t *testing.T) {
	got := nonEmptyLines("a\n\n  \n b \n")
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if nonEmptyLines("") != nil {
		t.Errorf("empty input should return nil")
	}
}

func TestChildBuses(t *testing.T) {
	got := childBuses([]string{"0000:c1:00.0", "0000:c1:00.1", "0000:bf:00.0", "garbage"})
	want := []string{"c1", "bf"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if childBuses(nil) != nil {
		t.Errorf("nil input should return nil")
	}
}

func TestTreeLinesFor(t *testing.T) {
	// Subtree shape from the width runbook (Confluence 1616642163) example.
	tree := strings.Join([]string{
		` \-01.0-[bd-c2]----00.0-[be-c2]--+-00.0-[bf]----00.0  Mellanox ConnectX-7`,
		`                                 +-01.0-[c0]----00.0  NVIDIA Device 2901`,
		`                                 +-02.0-[c1]----00.0  Solidigm NVMe DC SSD`,
		`                                 \-1f.0-[c2]----00.0  Broadcom PEX mgmt endpoint`,
		` +-02.0-[17]----00.0  Intel NVMe (unrelated)`,
	}, "\n")

	got := treeLinesFor(tree, []string{"c1"})
	want := []string{`                                 +-02.0-[c1]----00.0  Solidigm NVMe DC SSD`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leaf bus: got %q want %q", got, want)
	}

	// A bus that starts a range ([be-c2]) matches via the "[be-" form.
	got = treeLinesFor(tree, []string{"be"})
	want = []string{` \-01.0-[bd-c2]----00.0-[be-c2]--+-00.0-[bf]----00.0  Mellanox ConnectX-7`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("range bus: got %q want %q", got, want)
	}

	if lines := treeLinesFor(tree, []string{"zz"}); lines != nil {
		t.Errorf("unknown bus should match nothing, got %q", lines)
	}
}

func TestCheatSheet(t *testing.T) {
	cases := []struct {
		speed, width, down bool
		wantSub            string
	}{
		{false, false, false, ""},
		{true, true, false, "speed AND width below LnkCap"},
		{false, true, false, "lane failure"},
		{true, false, false, "signal integrity"},
		{true, true, true, "link down (x0)"},
		{false, false, true, "link down (x0)"},
	}
	for _, c := range cases {
		got := cheatSheet(c.speed, c.width, c.down)
		if c.wantSub == "" {
			if got != "" {
				t.Errorf("cheatSheet(%v,%v,%v) = %q, want empty", c.speed, c.width, c.down, got)
			}
			continue
		}
		if !strings.Contains(got, c.wantSub) {
			t.Errorf("cheatSheet(%v,%v,%v) = %q, want substring %q", c.speed, c.width, c.down, got, c.wantSub)
		}
	}
}
