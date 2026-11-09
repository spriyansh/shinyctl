package process

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTableIn_Linux(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string) {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt
	// utime stime cutime cstime priority nice threads itrealvalue starttime
	write("100/stat", "100 (R) S 1 100 100 0 -1 4194560 1 0 0 0 1 1 0 0 20 0 1 0 5000 1000 100 18446744073709551615\n")
	write("101/stat", "101 (a (b) c d) S 100 100 100 0 -1 4194560 1 0 0 0 1 1 0 0 20 0 1 0 5100 1000 100 18446744073709551615\n")
	write("102/stat", "102 (defunct) Z 1 100 100 0 -1 4228096 1 0 0 0 1 1 0 0 20 0 1 0 5200 0 0 18446744073709551615\n")
	write("103/stat", "103 (gone) X 1 103 103 0 -1 4228096 1 0 0 0 1 1 0 0 20 0 1 0 5300 0 0 18446744073709551615\n")
	write("104/stat", "garbage\n")
	write("105/other", "no stat file here")
	write("self/stat", "not a number as a directory name is skipped")
	write("net/tcp", "")

	table, err := tableIn(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]procInfo{}
	for _, p := range table {
		got[p.pid] = p
	}
	want := map[int]procInfo{
		100: {pid: 100, ppid: 1, pgid: 100, start: 5000},
		101: {pid: 101, ppid: 100, pgid: 100, start: 5100}, // the name holds spaces and parentheses
		102: {pid: 102, ppid: 1, pgid: 100, start: 5200, zombie: true},
		103: {pid: 103, ppid: 1, pgid: 103, start: 5300, zombie: true},
	}
	if len(got) != len(want) {
		t.Fatalf("table = %+v, want the %d readable processes", table, len(want))
	}
	for pid, w := range want {
		if got[pid] != w {
			t.Errorf("process %d = %+v, want %+v", pid, got[pid], w)
		}
	}

	if _, err := tableIn(filepath.Join(root, "nowhere")); err == nil {
		t.Error("a /proc that can't be listed is not an error")
	}
}
