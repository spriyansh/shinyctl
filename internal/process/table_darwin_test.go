package process

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func kinfo(pid, ppid, pgid int32, stat int8, sec int64, usec int32) unix.KinfoProc {
	var k unix.KinfoProc
	k.Proc.P_pid = pid
	k.Proc.P_stat = stat
	k.Proc.P_starttime.Sec = sec
	k.Proc.P_starttime.Usec = usec
	k.Eproc.Ppid = ppid
	k.Eproc.Pgid = pgid
	return k
}

func TestPlatformTable_Darwin(t *testing.T) {
	old := kinfoProcs
	defer func() { kinfoProcs = old }()

	kinfoProcs = func() ([]unix.KinfoProc, error) {
		return []unix.KinfoProc{
			kinfo(100, 1, 100, 2, 1700000000, 250000),
			kinfo(101, 100, 100, sZomb, 1700000001, 5),
		}, nil
	}
	table, err := platformTable()
	if err != nil {
		t.Fatal(err)
	}
	want := []procInfo{
		{pid: 100, ppid: 1, pgid: 100, start: 1700000000250000},
		{pid: 101, ppid: 100, pgid: 100, start: 1700000001000005, zombie: true},
	}
	if len(table) != len(want) || table[0] != want[0] || table[1] != want[1] {
		t.Errorf("table = %+v, want %+v", table, want)
	}

	kinfoProcs = func() ([]unix.KinfoProc, error) { return nil, errors.New("sysctl failed") }
	if _, err := platformTable(); err == nil {
		t.Error("a sysctl that fails is not an error")
	}
}
