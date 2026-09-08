package main

import "testing"

func TestScanFilterRMA(t *testing.T) {
	rows := []scanRow{
		{Name: "n1", State: "rma"},
		{Name: "n1", State: "rma"}, // second alert row for the same node
		{Name: "n2", State: "prepare-for-rma"},
		{Name: "n3", State: "triage"},
		{Name: "n4", State: "production"},
		{Name: "n5", State: "format"}, // contains "rma" as a substring — must be kept
		{Name: "n6", State: "-"},
	}
	kept, hidden := scanFilterRMA(rows)
	if hidden != 2 {
		t.Errorf("hidden = %d, want 2 (n1, n2)", hidden)
	}
	want := []string{"n3", "n4", "n5", "n6"}
	if len(kept) != len(want) {
		t.Fatalf("kept %d rows, want %d: %+v", len(kept), len(want), kept)
	}
	for i, name := range want {
		if kept[i].Name != name {
			t.Errorf("kept[%d].Name = %q, want %q", i, kept[i].Name, name)
		}
	}
}

func TestParseIgnoreRMA(t *testing.T) {
	o, err := parseArgs([]string{"--ignore-rma"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !o.ignoreRMA {
		t.Error("ignoreRMA not set by --ignore-rma")
	}
	o, err = parseArgs(nil)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if o.ignoreRMA {
		t.Error("ignoreRMA set without the flag")
	}
}
